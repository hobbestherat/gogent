package ui

import (
	"fmt"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// showSkillsDialog lists the loaded skills with their usage stats and a checkbox
// to toggle each one active. Active skills are advertised in the agent system
// prompt and loadable via the `skill` tool. Changes apply on OK.
func (w *Workbench) showSkillsDialog() {
	if w.handlers.GetSkills == nil {
		tv.ShowConfirmYesNo(w.desktop, "Skills", "Skills are unavailable.", nil)
		return
	}
	skills := w.handlers.GetSkills()
	if len(skills) == 0 {
		tv.ShowConfirmYesNo(w.desktop, "Skills",
			"No skills loaded.\nAdd SKILL.md folders under ~/.gogent/skills or ./skills.", nil)
		return
	}

	const width = 64
	rows := len(skills)
	height := rows + 7
	if height > w.app.Height() {
		height = w.app.Height()
	}
	x, y := centeredDialog(w, width, height)

	dialog := tv.NewDialog("Skills", x, y, width, height)
	dialog.Window.ShowClose = false

	header := dialogLabel("Active skills are listed to the model and loadable via the skill tool.",
		tv.Rect{X: 2, Y: 1, W: width - 4, H: 1})
	dialog.Window.AddContent(header)

	checks := make([]*tv.Checkbox, len(skills))
	for i, sk := range skills {
		label := fmt.Sprintf("%-16s ok:%d fail:%d", truncate(sk.Name, 16), sk.Success, sk.Failure)
		cb := tv.NewCheckbox("&"+label, tv.Rect{X: 2, Y: 2 + i, W: width - 4, H: 1}, nil)
		cb.FG = tv.DefaultTheme.DialogFG
		cb.BG = tv.DefaultTheme.DialogBG
		cb.SetChecked(sk.Active)
		checks[i] = cb
		dialog.Window.AddContent(cb)
	}

	var layer *tv.Layer
	apply := func() {
		if w.handlers.SetSkillActive != nil {
			for i, sk := range skills {
				if checks[i].IsChecked() != sk.Active {
					w.handlers.SetSkillActive(sk.Name, checks[i].IsChecked())
				}
			}
		}
		w.desktop.RemoveLayer(layer)
	}
	cancel := func() { w.desktop.RemoveLayer(layer) }

	ok := tv.NewButton("OK", tv.Rect{X: width - 24, Y: height - 3, W: 9, H: 1}, apply)
	cancelBtn := tv.NewButton("Cancel", tv.Rect{X: width - 13, Y: height - 3, W: 10, H: 1}, cancel)
	dialog.Window.AddContent(ok)
	dialog.Window.AddContent(cancelBtn)

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			cancel()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("skills-dialog", dialog)
	w.desktop.AddLayer(layer)
	if len(checks) > 0 {
		w.desktop.SetFocus(checks[0])
	}
}
