# shared-switch

Viam module for a reference-counted GPIO switch. Multiple clients `acquire` and `release` the switch; the pin stays energized as long as any client holds it and de-energizes when the last one releases. Useful when a shared load (e.g., a vacuum shared by two arms) needs to stay on as long as at least one client is using it.

## Models

- `viam:shared-switch:refcounted` (`rdk:component:switch`) — ref-counted switch backed by a board GPIO pin.

## DoCommand contract

- `{"op": "acquire", "client_id": "<id>"}` — register a holder.
- `{"op": "release", "client_id": "<id>"}` — remove a holder.
- `{"op": "status"}` — returns pin state, holders, and manual-override flag.
- `{"op": "clear"}` — drop all holders (recovery for a client that crashed without releasing).
- `{"op": "auto"}` — clear the manual override and resume holder-driven mode.

`SetPosition(0|1)` sets a hard manual override that pins the switch until cleared with `{"op": "auto"}`. `GetPosition()` returns the current pin state (0 or 1). `GetNumberOfPositions()` returns 2 (`off`, `on`).
