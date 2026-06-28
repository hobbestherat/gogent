package ui

// comfortableMaxWidth caps the default/open width of session windows and the
// three otherwise-uncapped dialogs (Resources browser, Command palette, Help
// overlay) on very wide terminals (issue #552). It governs INITIAL/auto sizing
// only — manual resize, drag, maximize, maximize-all and tiling still fill the
// area, and the boundary-only clamps (clampWindowRect, clampWindowSize,
// maximizedWindowRect, tileArea, sidebar clamps) are never constrained by it.
// 120 matches the review-viewer / agent-monologue precedent for code-bearing
// panes: it holds a 100-col code line plus chrome slack, sits under the
// readability ceiling (~130), and is a no-op under typical tmux pane splits
// (<=120), so it only bites the genuine wide-terminal sprawl case.
const comfortableMaxWidth = 120
