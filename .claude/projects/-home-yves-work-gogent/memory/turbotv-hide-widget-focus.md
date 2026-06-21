---
name: turbotv-hide-widget-focus
description: In turbotv, hiding a widget with zero bounds does NOT remove it from Tab focus — set Component.Visible=false
metadata:
  type: reference
---

In the turbotui/turbotv toolkit (used by gogent's `ui/tui`), hiding a widget by
giving it zero bounds (`SetBounds(tv.Rect{})`) makes it invisible and unclickable
but **leaves it in the Tab-focus cycle**: `collectFocusable` (turbotv
`component.go`) early-returns only on `!Visible || !Enabled`, never on empty
bounds, and `NewButton`/most widgets default to `Visible=true, Focusable=true`.

Consequence: a zero-bounds-but-Visible button can still catch a Tab and swallow
Enter/Space — e.g. an invisible Stop button intercepting a message send. To hide a
widget properly, set `b.Component.Visible = false` (zero the bounds too if layout
assertions want it). See `hideRunningButton` in `ui/tui/session_window.go` (issue
#201). The effort-control hide (`layoutEffortControl`) has the same latent pattern
— relevant when touching `session_window.go` (e.g. issue #195 status delineation).
