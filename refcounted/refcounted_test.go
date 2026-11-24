package refcounted

import (
	"context"
	"sync"
	"testing"
	"time"

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
	_, err := r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeTrue)
}

func TestAcquireReassertsPinOnDuplicate(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	_, err := r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	test.That(t, err, test.ShouldBeNil)
	initial := pin.writes()
	_, err = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, pin.writes(), test.ShouldEqual, initial+1)
	test.That(t, pin.isHigh(), test.ShouldBeTrue)
	test.That(t, len(r.holders), test.ShouldEqual, 1)
}

func TestTwoClientsShareOn(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	_, err := r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	test.That(t, err, test.ShouldBeNil)
	_, err = r.acquire(ctx, map[string]interface{}{"client_id": "arm2"})
	test.That(t, err, test.ShouldBeNil)
	_, err = r.release(ctx, map[string]interface{}{"client_id": "arm1"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeTrue)
	_, err = r.release(ctx, map[string]interface{}{"client_id": "arm2"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeFalse)
}

func TestReleaseUnknownClientIsNoop(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	_, err := r.release(ctx, map[string]interface{}{"client_id": "nobody"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeFalse)
	test.That(t, len(r.holders), test.ShouldEqual, 0)
}

func TestClearDropsAllHolders(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	_, _ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	_, _ = r.acquire(ctx, map[string]interface{}{"client_id": "arm2"})
	_, err := r.clear(ctx)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeFalse)
	test.That(t, len(r.holders), test.ShouldEqual, 0)
}

func TestClearAlsoDropsManualHolder(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	_ = r.SetPosition(ctx, positionOn, nil)
	_, _ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	test.That(t, len(r.holders), test.ShouldEqual, 2)
	_, err := r.clear(ctx)
	test.That(t, err, test.ShouldBeNil)
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
	_, _ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
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
	_, err := r.acquire(context.Background(), map[string]interface{}{"client_id": manualHolder})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "reserved")
}

func TestReleaseRejectsReservedManual(t *testing.T) {
	r, _ := newSwitch(t, true)
	_, err := r.release(context.Background(), map[string]interface{}{"client_id": manualHolder})
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
	_, _ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
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
	_, _ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
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

	_, err := r.acquire(context.Background(), map[string]interface{}{"client_id": "arm1"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "mismatch")
}

// --- Close ---

func TestCloseDeEnergizes(t *testing.T) {
	r, pin := newSwitch(t, true)
	ctx := context.Background()
	_, _ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	test.That(t, pin.isHigh(), test.ShouldBeTrue)
	test.That(t, r.Close(ctx), test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeFalse)
}

func TestCloseActiveLow(t *testing.T) {
	r, pin := newSwitch(t, false)
	test.That(t, r.Close(context.Background()), test.ShouldBeNil)
	test.That(t, pin.isHigh(), test.ShouldBeTrue) // de-energized under active-low → pin high
}

// --- DoCommand status response ---

func TestAcquireReleaseReturnStatus(t *testing.T) {
	r, _ := newSwitch(t, true)
	ctx := context.Background()

	resp, err := r.DoCommand(ctx, map[string]interface{}{"command": "acquire", "client_id": "arm2"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp, test.ShouldResemble, map[string]interface{}{"state": "on", "holders": []interface{}{"arm2"}})

	resp, err = r.DoCommand(ctx, map[string]interface{}{"command": "acquire", "client_id": "arm1"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp, test.ShouldResemble, map[string]interface{}{"state": "on", "holders": []interface{}{"arm1", "arm2"}})

	resp, err = r.DoCommand(ctx, map[string]interface{}{"command": "release", "client_id": "arm2"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp, test.ShouldResemble, map[string]interface{}{"state": "on", "holders": []interface{}{"arm1"}})

	resp, err = r.DoCommand(ctx, map[string]interface{}{"command": "release", "client_id": "arm1"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp, test.ShouldResemble, map[string]interface{}{"state": "off", "holders": []interface{}{}})
}

func TestReleaseUnknownClientReturnsStatus(t *testing.T) {
	r, _ := newSwitch(t, true)
	ctx := context.Background()
	_, _ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	resp, err := r.DoCommand(ctx, map[string]interface{}{"command": "release", "client_id": "nobody"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp, test.ShouldResemble, map[string]interface{}{"state": "on", "holders": []interface{}{"arm1"}})
}

func TestClearReturnsStatus(t *testing.T) {
	r, _ := newSwitch(t, true)
	ctx := context.Background()
	_, _ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	resp, err := r.DoCommand(ctx, map[string]interface{}{"command": "clear"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp, test.ShouldResemble, map[string]interface{}{"state": "off", "holders": []interface{}{}})
}

func TestAcquireFailureReturnsNilResponse(t *testing.T) {
	r := &refcounted{
		Named:      resource.NewName(toggleswitch.API, "test").AsNamed(),
		logger:     logging.NewTestLogger(t),
		pin:        &stuckLowPin{},
		activeHigh: true,
		holders:    make(map[string]struct{}),
	}
	resp, err := r.DoCommand(context.Background(), map[string]interface{}{"command": "acquire", "client_id": "arm1"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, resp, test.ShouldBeNil)
}

// --- Periodic status log ---

func TestLogStatusReportsStateAndHolders(t *testing.T) {
	r, _ := newSwitch(t, true)
	logger, logs := logging.NewObservedTestLogger(t)
	r.logger = logger
	r.pinName = "15"
	ctx := context.Background()
	_, _ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	_ = r.SetPosition(ctx, positionOn, nil)

	r.logStatus(ctx)
	entries := logs.FilterMessageSnippet("status:").All()
	test.That(t, len(entries), test.ShouldEqual, 1)
	test.That(t, entries[0].Message, test.ShouldEqual, `status: state=on holders=[arm1 manual] pin "15" high=true`)
}

func TestLogStatusWarnsOnPinMismatch(t *testing.T) {
	r, pin := newSwitch(t, true)
	logger, logs := logging.NewObservedTestLogger(t)
	r.logger = logger
	ctx := context.Background()
	_, _ = r.acquire(ctx, map[string]interface{}{"client_id": "arm1"})
	_ = pin.Set(ctx, false, nil)

	r.logStatus(ctx)
	entries := logs.FilterMessageSnippet("expected high=true").All()
	test.That(t, len(entries), test.ShouldEqual, 1)
	test.That(t, entries[0].Level.String(), test.ShouldEqual, "warn")
}

func TestStatusLogRunsPeriodicallyAndStopsOnClose(t *testing.T) {
	r, _ := newSwitch(t, true)
	logger, logs := logging.NewObservedTestLogger(t)
	r.logger = logger
	r.startStatusLog(10 * time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for logs.FilterMessageSnippet("status:").Len() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	test.That(t, logs.FilterMessageSnippet("status:").Len(), test.ShouldBeGreaterThanOrEqualTo, 2)

	test.That(t, r.Close(context.Background()), test.ShouldBeNil)
	n := logs.FilterMessageSnippet("status:").Len()
	time.Sleep(50 * time.Millisecond)
	test.That(t, logs.FilterMessageSnippet("status:").Len(), test.ShouldEqual, n)
}
