package refcounted

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.viam.com/rdk/components/board"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/test"
)

type fakePin struct {
	board.GPIOPin
	mu       sync.Mutex
	high     bool
	setCalls int
}

func (p *fakePin) Set(_ context.Context, high bool, _ map[string]interface{}) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.high = high
	p.setCalls++
	return nil
}

func (p *fakePin) Get(_ context.Context, _ map[string]interface{}) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.high, nil
}

func (p *fakePin) isHigh() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.high
}

func (p *fakePin) writes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.setCalls
}

func newSwitch(t *testing.T, activeHigh bool) (*refcounted, *fakePin) {
	t.Helper()
	pin := &fakePin{}
	r := &refcounted{
		Named:      resource.NewName(toggleswitch.API, "test").AsNamed(),
		logger:     logging.NewTestLogger(t),
		pin:        pin,
		activeHigh: activeHigh,
		holders:    make(map[string]struct{}),
	}
	r.mu.Lock()
	err := r.applyLocked(context.Background())
	r.mu.Unlock()
	test.That(t, err, test.ShouldBeNil)
	return r, pin
}

// --- Config.Validate ---

func TestValidateHappyPath(t *testing.T) {
	deps, _, err := (&Config{Board: "pi-board", Pin: "15"}).Validate("path")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"pi-board"})
}

func TestValidateRejectsEmptyBoard(t *testing.T) {
	_, _, err := (&Config{Pin: "15"}).Validate("path")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "board")
}

func TestValidateRejectsEmptyPin(t *testing.T) {
	_, _, err := (&Config{Board: "pi-board"}).Validate("path")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "pin")
}

func TestActiveHighDefaultsTrue(t *testing.T) {
	test.That(t, (&Config{}).activeHigh(), test.ShouldBeTrue)
	v := false
	test.That(t, (&Config{ActiveHigh: &v}).activeHigh(), test.ShouldBeFalse)
}

// --- Acquire / release lifecycle ---

func TestInitialStateOff(t *testing.T) {
	_, pin := newSwitch(t, true)
	test.That(t, pin.isHigh(), test.ShouldBeFalse)
}

func TestAcquireEnergizes(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	test.That(t, r.acquire(ctx, map[string]interface{}{"client_id": "arm1"}), test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeTrue)
}

func TestAcquireReassertsPinOnDuplicate(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	test.That(t, r.acquire(ctx, map[string]interface{}{"client_id": "arm1"}), test.ShouldBeNil)
	initial := pin.writes()
	test.That(t, r.acquire(ctx, map[string]interface{}{"client_id": "arm1"}), test.ShouldBeNil)
	test.That(t, pin.writes(), test.ShouldEqual, initial+1)
	test.That(t, pin.isHigh(), test.ShouldBeTrue)
	test.That(t, len(r.holders), test.ShouldEqual, 1)
}

func TestTwoClientsShareOn(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	test.That(t, r.acquire(ctx, map[string]interface{}{"client_id": "arm1"}), test.ShouldBeNil)
	test.That(t, r.acquire(ctx, map[string]interface{}{"client_id": "arm2"}), test.ShouldBeNil)
	test.That(t, r.release(ctx, map[string]interface{}{"client_id": "arm1"}), test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeTrue)
	test.That(t, r.release(ctx, map[string]interface{}{"client_id": "arm2"}), test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeFalse)
}

func TestReleaseUnknownClientIsNoop(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	test.That(t, r.release(ctx, map[string]interface{}{"client_id": "nobody"}), test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeFalse)
	test.That(t, len(r.holders), test.ShouldEqual, 0)
}

func TestClearDropsAllHolders(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	_ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	_ = r.acquire(ctx, map[string]interface{}{"client_id": "arm2"})
	test.That(t, r.clear(ctx), test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeFalse)
	test.That(t, len(r.holders), test.ShouldEqual, 0)
}

func TestClearAlsoDropsManualHolder(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	_ = r.SetPosition(ctx, positionOn, nil)
	_ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	test.That(t, len(r.holders), test.ShouldEqual, 2)
	test.That(t, r.clear(ctx), test.ShouldBeNil)
	test.That(t, len(r.holders), test.ShouldEqual, 0)
	test.That(t, pin.isHigh(), test.ShouldBeFalse)
}

// --- SetPosition adds/removes the manual virtual holder ---

func TestSetPositionOnAddsManualHolder(t *testing.T) {
	r, pin := newSwitch(t, true)
	test.That(t, r.SetPosition(context.Background(), positionOn, nil), test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeTrue)
	_, held := r.holders[manualHolder]
	test.That(t, held, test.ShouldBeTrue)
}

func TestSetPositionOffRemovesManualHolder(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	_ = r.SetPosition(ctx, positionOn, nil)
	test.That(t, r.SetPosition(ctx, positionOff, nil), test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeFalse)
	test.That(t, len(r.holders), test.ShouldEqual, 0)
}

func TestSetPositionOffKeepsClientHolders(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	_ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	_ = r.SetPosition(ctx, positionOn, nil) // adds manual
	test.That(t, r.SetPosition(ctx, positionOff, nil), test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeTrue) // arm1 still holds
	_, armHeld := r.holders["arm1"]
	test.That(t, armHeld, test.ShouldBeTrue)
	_, manualHeld := r.holders[manualHolder]
	test.That(t, manualHeld, test.ShouldBeFalse)
}

func TestSetPositionOnReassertsPinOnDuplicate(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	_ = r.SetPosition(ctx, positionOn, nil)
	initial := pin.writes()
	_ = r.SetPosition(ctx, positionOn, nil)
	test.That(t, pin.writes(), test.ShouldEqual, initial+1)
	test.That(t, pin.isHigh(), test.ShouldBeTrue)
}

func TestSetPositionOffWithoutManualIsNoop(t *testing.T) {
	r, pin := newSwitch(t, true)
	initial := pin.writes()
	test.That(t, r.SetPosition(context.Background(), positionOff, nil), test.ShouldBeNil)
	test.That(t, pin.writes(), test.ShouldEqual, initial)
}

func TestSetPositionRejectsInvalid(t *testing.T) {
	r, _ := newSwitch(t, true)
	err := r.SetPosition(context.Background(), 42, nil)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "invalid position")
}

// --- Reserved client_id ---

func TestAcquireRejectsReservedManual(t *testing.T) {
	r, _ := newSwitch(t, true)
	err := r.acquire(context.Background(), map[string]interface{}{"client_id": manualHolder})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "reserved")
}

func TestReleaseRejectsReservedManual(t *testing.T) {
	r, _ := newSwitch(t, true)
	err := r.release(context.Background(), map[string]interface{}{"client_id": manualHolder})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "reserved")
}

// --- Switch API surface ---

func TestGetPositionReflectsHolders(t *testing.T) {
	r, _ := newSwitch(t, true)
	ctx := context.Background()
	pos, err := r.GetPosition(ctx, nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, pos, test.ShouldEqual, positionOff)
	_ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	pos, _ = r.GetPosition(ctx, nil)
	test.That(t, pos, test.ShouldEqual, positionOn)
}

func TestGetNumberOfPositions(t *testing.T) {
	r, _ := newSwitch(t, true)
	n, labels, err := r.GetNumberOfPositions(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, n, test.ShouldEqual, uint32(2))
	test.That(t, labels, test.ShouldResemble, []string{"off", "on"})
}

// --- Active-low wiring ---

func TestActiveLowInverts(t *testing.T) {
	r, pin := newSwitch(t, false)
	ctx := context.Background()
	test.That(t, pin.isHigh(), test.ShouldBeTrue) // off + active-low → pin high
	_ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	test.That(t, pin.isHigh(), test.ShouldBeFalse) // on + active-low → pin low
}

// --- DoCommand routing ---

func TestDoCommandMissingCommand(t *testing.T) {
	r, _ := newSwitch(t, true)
	_, err := r.DoCommand(context.Background(), map[string]interface{}{})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "command")
}

func TestDoCommandUnknownCommand(t *testing.T) {
	r, _ := newSwitch(t, true)
	_, err := r.DoCommand(context.Background(), map[string]interface{}{"command": "bogus"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "unknown command")
}

func TestDoCommandAcquireMissingClientID(t *testing.T) {
	r, _ := newSwitch(t, true)
	_, err := r.DoCommand(context.Background(), map[string]interface{}{"command": "acquire"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "client_id")
}

func TestDoCommandAcquireReleaseRoundtrip(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	_, err := r.DoCommand(ctx, map[string]interface{}{"command": "acquire", "client_id": "arm1"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeTrue)
	_, err = r.DoCommand(ctx, map[string]interface{}{"command": "release", "client_id": "arm1"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeFalse)
}

// --- Pin state read-back verification ---

type stuckLowPin struct {
	board.GPIOPin
}

func (p *stuckLowPin) Set(_ context.Context, _ bool, _ map[string]interface{}) error {
	return nil
}

func (p *stuckLowPin) Get(_ context.Context, _ map[string]interface{}) (bool, error) {
	return false, nil
}

func TestAcquireFailsWhenPinDidNotReachCommandedState(t *testing.T) {
	r := &refcounted{
		Named:      resource.NewName(toggleswitch.API, "test").AsNamed(),
		logger:     logging.NewTestLogger(t),
		boardName:  "test-board",
		pinName:    "test-pin",
		pin:        &stuckLowPin{},
		activeHigh: true,
		holders:    make(map[string]struct{}),
	}
	r.mu.Lock()
	initErr := r.applyLocked(context.Background())
	r.mu.Unlock()
	test.That(t, initErr, test.ShouldBeNil)

	err := r.acquire(context.Background(), map[string]interface{}{"client_id": "arm1"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "mismatch")
}

// --- Close ---

func TestCloseDeEnergizes(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	_ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	test.That(t, pin.isHigh(), test.ShouldBeTrue)
	test.That(t, r.Close(ctx), test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeFalse)
}

func TestCloseActiveLow(t *testing.T) {
	r, pin := newSwitch(t, false)
	test.That(t, r.Close(context.Background()), test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeTrue) // de-energized under active-low → pin high
}

// --- persistence ---

func newPersistentSwitch(t *testing.T, path string) (*refcounted, *fakePin) {
	t.Helper()
	holders, err := loadHolders(path)
	test.That(t, err, test.ShouldBeNil)
	r, pin := newSwitch(t, true)
	r.holdersPath = path
	r.holders = holders
	r.mu.Lock()
	err = r.applyLocked(context.Background())
	r.mu.Unlock()
	test.That(t, err, test.ShouldBeNil)
	return r, pin
}

func TestHoldersSurviveRebuild(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vacuum.holders.json")

	first, _ := newPersistentSwitch(t, path)
	test.That(t, first.acquire(ctx, map[string]interface{}{"client_id": "arm-a"}), test.ShouldBeNil)
	test.That(t, first.acquire(ctx, map[string]interface{}{"client_id": "arm-b"}), test.ShouldBeNil)
	test.That(t, first.Close(ctx), test.ShouldBeNil)

	second, pin := newPersistentSwitch(t, path)
	test.That(t, pin.isHigh(), test.ShouldBeTrue)
	test.That(t, second.sortedHoldersLocked(), test.ShouldResemble, []string{"arm-a", "arm-b"})

	test.That(t, second.release(ctx, map[string]interface{}{"client_id": "arm-a"}), test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeTrue)
	test.That(t, second.release(ctx, map[string]interface{}{"client_id": "arm-b"}), test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeFalse)

	third, pin := newPersistentSwitch(t, path)
	test.That(t, third.sortedHoldersLocked(), test.ShouldBeEmpty)
	test.That(t, pin.isHigh(), test.ShouldBeFalse)
}

func TestClearIsPersisted(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vacuum.holders.json")
	r, _ := newPersistentSwitch(t, path)
	test.That(t, r.acquire(ctx, map[string]interface{}{"client_id": "arm-a"}), test.ShouldBeNil)
	test.That(t, r.clear(ctx), test.ShouldBeNil)

	holders, err := loadHolders(path)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, holders, test.ShouldBeEmpty)
}

func TestAcquireRollsBackWhenPersistFails(t *testing.T) {
	ctx := context.Background()
	r, pin := newPersistentSwitch(t, filepath.Join(t.TempDir(), "missing-dir", "vacuum.holders.json"))
	err := r.acquire(ctx, map[string]interface{}{"client_id": "arm-a"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, r.sortedHoldersLocked(), test.ShouldBeEmpty)
	test.That(t, pin.isHigh(), test.ShouldBeFalse)
}

func TestReleaseRollsBackWhenPersistFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r, pin := newPersistentSwitch(t, filepath.Join(dir, "vacuum.holders.json"))
	test.That(t, r.acquire(ctx, map[string]interface{}{"client_id": "arm-a"}), test.ShouldBeNil)
	r.holdersPath = filepath.Join(dir, "missing-dir", "vacuum.holders.json")

	err := r.release(ctx, map[string]interface{}{"client_id": "arm-a"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, r.sortedHoldersLocked(), test.ShouldResemble, []string{"arm-a"})
	test.That(t, pin.isHigh(), test.ShouldBeTrue)
}

func TestLoadHoldersRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vacuum.holders.json")
	test.That(t, os.WriteFile(path, []byte("{not json"), 0o600), test.ShouldBeNil)
	_, err := loadHolders(path)
	test.That(t, err, test.ShouldNotBeNil)
}

func TestLoadHoldersMissingFileIsEmpty(t *testing.T) {
	holders, err := loadHolders(filepath.Join(t.TempDir(), "absent.json"))
	test.That(t, err, test.ShouldBeNil)
	test.That(t, holders, test.ShouldBeEmpty)
}
