package ui

import (
	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// This file holds the per-session extended-thinking control — the header toggle
// symmetric with the reasoning-effort selector (see selectedEffort /
// rebuildEffortOptions in session_window.go). It is gated on the selected model's
// Caps.ThinkingToggle: a model that exposes no explicit thinking switch shows the
// control greyed out. "(default)" means no override (use the model config's
// thinking value); "on"/"off" force it for this turn only.

// thinkLabelWidth is the width reserved for the "Think" label to the left of the
// thinking selector.
const thinkLabelWidth = 6

// selectedThinking returns the per-session thinking override for the current
// selection: "on"/"off", or "" for "(default)" (and for a disabled selector),
// meaning "use the model config's thinking value".
func (sw *SessionWindow) selectedThinking() string {
	if sw == nil || sw.thinkSelect == nil || !sw.thinkEnabled {
		return ""
	}
	v := sw.thinkSelect.Value()
	if v == effortDefaultOption {
		return ""
	}
	return v
}

// rebuildThinkOptions enables the thinking control only when the selected model
// advertises a thinking toggle (Caps.ThinkingToggle), and seeds the selection from
// the model's configured Thinking pointer (nil => default, true => on, false =>
// off) — unless the user already made an explicit pick that still applies.
func (sw *SessionWindow) rebuildThinkOptions() {
	if sw.thinkSelect == nil {
		return
	}
	prev := sw.thinkSelect.Value()
	cfg := sw.selectedModelConfig()
	sw.thinkEnabled = cfg != nil && cfg.Caps.ThinkingToggle
	if sw.thinkLabel != nil {
		if sw.thinkEnabled {
			sw.thinkLabel.FG = sw.thinkLabelEnabledFG
		} else {
			sw.thinkLabel.FG = colorNote
		}
	}
	// Preserve an explicit prior pick ("on"/"off"); otherwise seed from the model's
	// configured Thinking pointer; otherwise "(default)".
	sw.thinkSelect.Selected = 0
	switch {
	case prev == "on":
		sw.thinkSelect.Selected = 1
	case prev == "off":
		sw.thinkSelect.Selected = 2
	case cfg != nil && cfg.Thinking != nil && *cfg.Thinking:
		sw.thinkSelect.Selected = 1
	case cfg != nil && cfg.Thinking != nil && !*cfg.Thinking:
		sw.thinkSelect.Selected = 2
	}
}

// guardThinkSelect makes a disabled (greyed-out) thinking control inert, mirroring
// guardEffortSelect.
func (sw *SessionWindow) guardThinkSelect() {
	c := sw.thinkSelect.Component
	baseClick := c.OnClickFn
	c.OnClickFn = func(vc *tv.VisualComponent, event tui.ClickEvent) bool {
		if !sw.thinkEnabled {
			return true
		}
		if baseClick != nil {
			return baseClick(vc, event)
		}
		return false
	}
	baseType := c.OnTypeFn
	c.OnTypeFn = func(vc *tv.VisualComponent, event tui.TypeEvent) bool {
		if !sw.thinkEnabled {
			return false
		}
		if baseType != nil {
			return baseType(vc, event)
		}
		return false
	}
}

// hideThinkControl collapses the thinking widget to zero bounds (used when the
// header is too narrow, alongside the effort control hiding).
func (sw *SessionWindow) hideThinkControl() {
	sw.thinkHidden = true
	if sw.thinkSelect != nil {
		sw.thinkSelect.Component.SetBounds(tv.Rect{})
	}
	if sw.thinkLabel != nil {
		sw.thinkLabel.Component.SetBounds(tv.Rect{})
	}
}

// layoutThinkControl places the thinking selector + label immediately left of the
// effort control (effortLabelX is the effort label's left edge), hiding it when it
// would overlap the model selector (right edge modelRight).
func (sw *SessionWindow) layoutThinkControl(wd, modelRight, effortLabelX int) {
	if sw.thinkSelect == nil || sw.thinkLabel == nil {
		return
	}
	const selW = 11 // fits "(default)" + ▼
	selX := effortLabelX - 1 - selW
	labelX := selX - thinkLabelWidth
	if labelX <= modelRight {
		sw.hideThinkControl()
		return
	}
	sw.thinkHidden = false
	sw.thinkSelect.Component.SetBounds(tv.Rect{X: selX, Y: 0, W: selW, H: 1})
	sw.thinkLabel.Component.SetBounds(tv.Rect{X: labelX, Y: 0, W: thinkLabelWidth, H: 1})
}
