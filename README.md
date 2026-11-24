# shared-switch

Viam module for a reference-counted GPIO switch. Multiple clients `acquire` and `release` the switch; the pin stays energized as long as any client holds it and de-energizes when the last one releases. Useful when a shared load (e.g., a vacuum shared by two arms) needs to stay on as long as at least one client is using it.

## Models

- `viam:shared-switch:refcounted` (`rdk:component:switch`) — ref-counted switch backed by a board GPIO pin.

### Config

```json
{
  "name": "vacuum",
  "model": "viam:shared-switch:refcounted",
  "attributes": {
    "board": "pi-board",
    "pin": "15",
    "active_high": true
  }
}
```

- `board` *(string, required)* — name of the `board` component providing the GPIO pin.
- `pin` *(string, required)* — pin name/number on that board.
- `active_high` *(bool, default `true`)* — set to `false` for active-low wiring.

## DoCommand contract

- `{"command": "acquire", "client_id": "<id>"}` — register a holder.
- `{"command": "release", "client_id": "<id>"}` — remove a holder.
- `{"command": "clear"}` — drop all holders (recovery for a client that crashed without releasing; also the emergency "everybody off" hook).

`client_id` must be non-empty and cannot be `"manual"` — that name is reserved for the UI toggle.

On success, all three commands return the switch status after the change:

```json
{"state": "on", "holders": ["arm1", "manual"]}
```

`state` is `"on"` or `"off"`; `holders` is the sorted list of client ids still holding the switch (empty when off), including `"manual"` if the UI toggle is on.

## Status log

Every minute the module logs the switch state, its holders, and the pin level read back from the board, e.g. `status: state=on holders=[arm1] pin "15" high=true`. If the pin level doesn't match what the holders imply (e.g. something else drove the pin), the line is logged as a warning with the expected level.

`SetPosition(1)` adds a virtual `"manual"` holder; `SetPosition(0)` removes it. `SetPosition(0)` does **not** stop the load if a real client is still holding — that's what `{"command":"clear"}` is for. `GetPosition()` returns `1` if any client holds, else `0`. `GetNumberOfPositions()` returns 2 (`off`, `on`).
