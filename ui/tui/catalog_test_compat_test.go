package ui

// command is the pre-#401 test name for catalog rows. Production intentionally
// removed the old command table; tests keep this alias so older behavior tests now
// exercise the unified actions() catalog instead of requiring a production shim.
type command = action

func (w *Workbench) commands() []command {
	return w.actions()
}

func keybindActions() []action {
	return (&Workbench{}).rebindable()
}
