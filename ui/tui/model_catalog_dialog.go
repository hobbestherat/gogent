package ui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gogent/internal/config"
	"gogent/internal/modelsdev"

	tui "github.com/hobbestherat/turbotui"
	tv "github.com/hobbestherat/turbotui/turbotv"
)

// This file implements the "Add model from catalog" flow (issue #486): a small
// wizard that fetches the models.dev catalog, lets the user pick a PROVIDER then a
// MODEL (both searchable), and opens a pre-filled, fully-editable review form that
// — on Save — creates a NEW model config via handlers.AddModel. Since issue #509
// the wizard is the "Add from Catalog…" path of the unified Models… dialog
// (showModelsDialog): the only addition is an onClose continuation threaded
// through the steps so finishing or cancelling the wizard returns the user to a
// fresh Models… list. When the catalog is unreachable every entry point degrades
// gracefully to that dialog's manual Add Empty… path.
//
// The catalog fetch can hit the network, so it always runs off the UI thread
// behind a "Loading catalog…" layer (mirroring the model form's scanModels
// goroutine), posting the result back via desktop.Post.

// catalogReady reports whether the catalog-assisted flow is wired (both the
// AddModel mutation and the catalog source). When false the affordance is hidden.
func (w *Workbench) catalogReady() bool {
	return w.handlers.AddModel != nil && w.handlers.GetModelCatalog != nil
}

// showAddModelDialog is the wizard entry point. It loads the catalog off-thread
// and, on success, opens the provider picker; on failure it offers the manual
// editor instead so the feature never blocks. onClose is the continuation invoked
// when the wizard finishes (added or cancelled) — the unified Models… dialog
// passes a closure that reopens a fresh list so control returns there; nil is a
// no-op. The empty-catalog path is handled by offerManualEditor (which itself
// reopens the Models dialog), so it does not also call onClose.
func (w *Workbench) showAddModelDialog(onClose func()) {
	if !w.catalogReady() {
		w.showConfirm("Add model", "Catalog-assisted model setup is unavailable.", nil)
		return
	}
	w.loadCatalogThen(false, onClose, func(cat modelsdev.Catalog, err error) {
		if len(cat) == 0 {
			w.offerManualEditor(err)
			return
		}
		w.showCatalogProviderStep(cat, onClose)
	})
}

// offerManualEditor surfaces an offline/empty-catalog state and offers to fall
// back to the existing manual model editor (graceful degradation).
func (w *Workbench) offerManualEditor(err error) {
	msg := "The models.dev catalog is unavailable"
	if err != nil {
		msg += ":\n" + err.Error()
	}
	if w.handlers.GetModels != nil && w.handlers.AddModel != nil {
		msg += "\n\nOpen the Models dialog to add one manually instead?"
		w.showConfirm("Add model", msg, func(ok bool) {
			if ok {
				w.showModelsDialog()
			}
		})
		return
	}
	w.showConfirm("Add model", msg, nil)
}

// loadCatalogThen shows a cancellable "Loading catalog…" modal, fetches the
// catalog on a goroutine (force triggers a revalidating refresh), and invokes
// onReady on the UI thread with the result. Cancel/Escape cancels the fetch's
// context (aborting an in-flight network GET) and drops the posted callback;
// onClose (nil => no-op) then runs so a load cancelled before any wizard step
// still returns the user to the Models… list (the caller removed it on entry).
func (w *Workbench) loadCatalogThen(force bool, onClose func(), onReady func(modelsdev.Catalog, error)) {
	spec := tv.DialogSpec{MinW: 40, MinH: 5, MaxH: 5, PreferredW: 40}
	x, y, width, height := w.dialogRect(spec)
	dialog := tv.NewDialog("Catalog", x, y, width, height)
	applyWindowShadow(dialog.Window)
	dialog.Window.ShowClose = false

	label := "Loading catalog…"
	if force {
		label = "Refreshing catalog…"
	}
	dialog.Window.AddContent(dialogLabel(label, tv.Rect{X: 2, Y: 1, W: width - 4, H: 1}))

	ctx, cancelFetch := context.WithCancel(context.Background())
	var layer *tv.Layer
	done := false
	cancel := func() {
		if done {
			return
		}
		done = true
		cancelFetch() // abort the in-flight GET, not just drop its result
		w.desktop.RemoveLayer(layer)
		if onClose != nil {
			onClose() // load abandoned: return to the Models… list
		}
	}
	dialog.Window.AddContent(newButton("Cancel", tv.Rect{X: width - 13, Y: height - 3, W: 10, H: 1}, cancel))
	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			cancel()
			return true
		}
		return false
	}
	layer = tv.NewModalLayer("catalog-loading", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec)

	go func() {
		defer cancelFetch() // release the context on the normal-completion path too
		cat, err := w.handlers.GetModelCatalog(ctx, force)
		w.desktop.Post(func() {
			if done {
				return // user cancelled the load
			}
			done = true
			w.desktop.RemoveLayer(layer)
			onReady(cat, err)
		})
	}()
}

// pickerStep describes one searchable list step of the wizard.
type pickerStep struct {
	title     string
	label     string       // the row label (e.g. "Provider:")
	rows      []string     // display strings
	keys      []string     // value returned for rows[i]
	onPick    func(string) // called with the chosen key
	onBack    func()       // nil => no Back button
	onRefresh func()       // nil => no Refresh button
	onClose   func()       // invoked when the step is cancelled/Escaped (nil => no-op)
}

// showPicker renders a filter TextBox above a Select. Typing in the filter
// narrows the Select's options (substring, case-insensitive); the user then Tabs
// to the Select to choose and presses Next. This composes existing turbotui
// primitives — no searchable-list widget is introduced.
func (w *Workbench) showPicker(step pickerStep) {
	spec := tv.DialogSpec{MinW: 72, MinH: 11, MaxH: 11, PreferredW: 80}
	x, y, width, height := w.dialogRect(spec)
	boxX := 2 + 10
	boxW := width - boxX - 3

	dialog := tv.NewDialog(step.title, x, y, width, height)
	applyWindowShadow(dialog.Window)
	dialog.Window.ShowClose = false

	dialog.Window.AddContent(dialogLabel("Filter:", tv.Rect{X: 2, Y: 1, W: 10, H: 1}))
	filter := tv.NewTextBox("", tv.Rect{X: boxX, Y: 1, W: boxW, H: 1})
	dialog.Window.AddContent(filter)

	dialog.Window.AddContent(dialogLabel(step.label, tv.Rect{X: 2, Y: 3, W: 10, H: 1}))
	sel := newSelect(w.desktop, nil, tv.Rect{X: boxX, Y: 3, W: boxW, H: 1})
	dialog.Window.AddContent(sel)

	currentKey := wireFilter(filter, sel, step.rows, step.keys)

	var layer *tv.Layer
	cancel := func() {
		w.desktop.RemoveLayer(layer)
		if step.onClose != nil {
			step.onClose() // return to the Models… list when the wizard is abandoned
		}
	}
	next := func() {
		key := currentKey()
		if key == "" {
			return
		}
		w.desktop.RemoveLayer(layer)
		step.onPick(key)
	}

	// Bottom button row: optional Back/Refresh on the left, Next + Cancel on the
	// right.
	leftX := 2
	if step.onBack != nil {
		bw := tv.ButtonLabelWidth("Back")
		dialog.Window.AddContent(newButton("Back", tv.Rect{X: leftX, Y: height - 3, W: bw, H: 1}, func() {
			w.desktop.RemoveLayer(layer)
			step.onBack()
		}))
		leftX += bw + 1
	}
	if step.onRefresh != nil {
		rw := tv.ButtonLabelWidth("Refresh")
		dialog.Window.AddContent(newButton("Refresh", tv.Rect{X: leftX, Y: height - 3, W: rw, H: 1}, func() {
			w.desktop.RemoveLayer(layer)
			step.onRefresh()
		}))
	}
	dialog.Window.AddContent(newButton("Next", tv.Rect{X: width - 24, Y: height - 3, W: 9, H: 1}, next))
	dialog.Window.AddContent(newButton("Cancel", tv.Rect{X: width - 13, Y: height - 3, W: 10, H: 1}, cancel))

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			cancel()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("catalog-picker", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec)
	w.desktop.SetFocus(filter)
}

// wireFilter connects a filter TextBox to a Select: every keystroke re-filters
// rows (substring, case-insensitive) and updates the Select. It returns a closure
// yielding the key behind the currently-selected (filtered) option. The TextBox
// has no change callback, so we chain its OnTypeFn after the original handler.
func wireFilter(filter *tv.TextBox, sel *tv.Select, rows, keys []string) func() string {
	filteredKeys := append([]string(nil), keys...)
	apply := func(q string) {
		q = strings.ToLower(strings.TrimSpace(q))
		fr := make([]string, 0, len(rows))
		fk := make([]string, 0, len(rows))
		for i, r := range rows {
			if q == "" || strings.Contains(strings.ToLower(r), q) {
				fr = append(fr, r)
				fk = append(fk, keys[i])
			}
		}
		filteredKeys = fk
		sel.SetOptions(fr)
	}
	orig := filter.Component.OnTypeFn
	filter.Component.OnTypeFn = func(vc *tv.VisualComponent, event tui.TypeEvent) bool {
		handled := orig(vc, event)
		apply(filter.GetText())
		return handled
	}
	apply("")
	return func() string {
		idx := sel.GetSelected()
		if idx < 0 || idx >= len(filteredKeys) {
			return ""
		}
		return filteredKeys[idx]
	}
}

// showCatalogProviderStep is wizard step 1: pick a provider. Each row shows the
// provider name and the env var(s) it expects, so the user knows what credential
// to have ready.
func (w *Workbench) showCatalogProviderStep(cat modelsdev.Catalog, onClose func()) {
	ids := make([]string, 0, len(cat))
	for id := range cat {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return strings.ToLower(providerLabel(cat[ids[i]])+ids[i]) < strings.ToLower(providerLabel(cat[ids[j]])+ids[j])
	})
	rows := make([]string, len(ids))
	for i, id := range ids {
		p := cat[id]
		env := "—"
		if len(p.Env) > 0 {
			env = strings.Join(p.Env, ", ")
		}
		rows[i] = fmt.Sprintf("%s — env: %s", providerLabel(p), env)
	}
	w.showPicker(pickerStep{
		title:  "Add model — provider",
		label:  "Provider:",
		rows:   rows,
		keys:   ids,
		onPick: func(id string) { w.showCatalogModelStep(cat, id, onClose) },
		onRefresh: func() {
			w.loadCatalogThen(true, onClose, func(fresh modelsdev.Catalog, err error) {
				if len(fresh) == 0 {
					w.offerManualEditor(err)
					return
				}
				w.showCatalogProviderStep(fresh, onClose)
			})
		},
		onClose: onClose,
	})
}

// showCatalogModelStep is wizard step 2: pick a model from the chosen provider.
// Each row shows display name, context window, output cap, and reasoning/free
// badges.
func (w *Workbench) showCatalogModelStep(cat modelsdev.Catalog, providerID string, onClose func()) {
	p := cat[providerID]
	ids := make([]string, 0, len(p.Models))
	for id := range p.Models {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return strings.ToLower(modelLabel(p.Models[ids[i]])+ids[i]) < strings.ToLower(modelLabel(p.Models[ids[j]])+ids[j])
	})
	rows := make([]string, len(ids))
	for i, id := range ids {
		rows[i] = modelRow(p.Models[id])
	}
	w.showPicker(pickerStep{
		title:  fmt.Sprintf("Add model — %s", providerLabel(p)),
		label:  "Model:",
		rows:   rows,
		keys:   ids,
		onPick: func(id string) { w.showCatalogReviewStep(cat, providerID, id, onClose) },
		onBack: func() { w.showCatalogProviderStep(cat, onClose) },
		// onBack returns to the provider step (still in the wizard); only an outright
		// cancel/Escape ends it, so onClose routes back to the Models… list there.
		onClose: onClose,
	})
}

// providerLabel is the provider's display name, falling back to its id.
func providerLabel(p modelsdev.Provider) string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

// modelLabel is the model's display name, falling back to its id.
func modelLabel(m modelsdev.Model) string {
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

// modelRow renders a model picker row: name plus key facts and badges.
func modelRow(m modelsdev.Model) string {
	row := modelLabel(m)
	if m.Limit.Context > 0 {
		row += fmt.Sprintf("  · ctx %s", tokensShort(m.Limit.Context))
	}
	if m.Limit.Output > 0 {
		row += fmt.Sprintf(" · out %s", tokensShort(m.Limit.Output))
	}
	if m.Reasoning || len(m.ReasoningOptions) > 0 {
		row += " · reasoning"
	}
	if m.Cost.Input == 0 && m.Cost.Output == 0 {
		row += " · free"
	}
	return row
}

// tokensShort renders a token count compactly (e.g. 200000 -> "200K", 1000000 ->
// "1M") for the model picker badges.
func tokensShort(n int) string {
	switch {
	case n >= 1_000_000 && n%1_000_000 == 0:
		return strconv.Itoa(n/1_000_000) + "M"
	case n >= 1000:
		return strconv.Itoa(n/1000) + "K"
	default:
		return strconv.Itoa(n)
	}
}

// showCatalogReviewStep is wizard step 3: a pre-filled, fully-editable review
// form. Every field models.dev can supply is auto-filled; API type is read-only
// ("from catalog"); the API key (or Vertex project/location) is blank and
// required. Save creates a NEW entry via handlers.AddModel and refreshes the app.
func (w *Workbench) showCatalogReviewStep(cat modelsdev.Catalog, providerID, modelID string, onClose func()) {
	p := cat[providerID]
	cm := p.Models[modelID]

	draft := modelsdev.ToModelConfig(providerID, p, cm)
	draft.Name = modelsdev.UniqueName(providerID, modelID, w.takenModelNames())
	isVertex := strings.HasPrefix(draft.APIType, "vertex")

	const labelW = 16
	const boxX = 2 + labelW
	spec := tv.DialogSpec{MinW: 70, MinH: 18, MaxH: 18, PreferredW: 84}
	x, y, width, height := w.dialogRect(spec)
	boxW := width - boxX - 3

	dialog := tv.NewDialog(fmt.Sprintf("Add model — %s", modelLabel(cm)), x, y, width, height)
	applyWindowShadow(dialog.Window)
	dialog.Window.ShowClose = false

	field := func(text string, row int) *tv.TextBox {
		dialog.Window.AddContent(dialogLabel(text, tv.Rect{X: 2, Y: row, W: labelW, H: 1}))
		box := tv.NewTextBox("", tv.Rect{X: boxX, Y: row, W: boxW, H: 1})
		dialog.Window.AddContent(box)
		return box
	}

	name := field("Name:", 1)
	name.SetText(draft.Name)

	// API type is read-only ("from catalog"): turbotui's Select has no disabled
	// state, so render the derived type as a static label rather than a control the
	// user can't meaningfully change. The value travels in the draft.
	dialog.Window.AddContent(dialogLabel("API type:", tv.Rect{X: 2, Y: 2, W: labelW, H: 1}))
	dialog.Window.AddContent(dialogLabel(draft.APIType+"  (from catalog)", tv.Rect{X: boxX, Y: 2, W: boxW, H: 1}))

	display := field("Display name:", 3)
	display.SetText(draft.DisplayName)
	endpoint := field("Endpoint:", 4)
	endpoint.SetText(draft.Endpoint)

	// Model id: a plain, editable text field pre-filled with the catalog
	// selection. Unlike the manual editor there is deliberately NO "Scan" button
	// here: the id is already the model the user just picked from the catalog, so
	// scanning is redundant — and a draft scan cannot work in remote mode anyway,
	// where ScanModels is keyed by a SAVED model name (the unsaved draft would
	// 404). The user can still hand-edit the id.
	modelIDBox := field("Model id:", 5)
	modelIDBox.SetText(draft.Model)

	apiKey := field("API key:", 6)
	temp := field("Temperature:", 7)
	temp.SetText(strconv.FormatFloat(float64(draft.Temperature), 'g', -1, 32))
	maxTokens := field("Max tokens:", 8)
	maxTokens.SetText(strconv.Itoa(draft.MaxTokens))
	reasoning := field("Reasoning:", 9)
	reasoning.SetText(draft.ReasoningEffort)

	dialog.Window.AddContent(dialogLabel("Thinking:", tv.Rect{X: 2, Y: 10, W: labelW, H: 1}))
	thinking := newSelect(w.desktop, []string{"default", "on", "off"}, tv.Rect{X: boxX, Y: 10, W: boxW, H: 1})
	thinking.SetSelected(thinkingIndex(draft.Thinking))
	dialog.Window.AddContent(thinking)

	project := field("Project:", 11)
	project.SetText(draft.Project)
	location := field("Location:", 12)
	location.SetText(draft.Location)

	var layer *tv.Layer
	cancel := func() {
		w.desktop.RemoveLayer(layer)
		if onClose != nil {
			onClose() // abandoned at the review step: return to the Models… list
		}
	}

	save := func() {
		draft.Name = strings.TrimSpace(name.GetText())
		draft.DisplayName = display.GetText()
		draft.Endpoint = strings.TrimSpace(endpoint.GetText())
		draft.Model = strings.TrimSpace(modelIDBox.GetText())
		draft.APIKey = apiKey.GetText()
		if v, err := strconv.ParseFloat(temp.GetText(), 32); err == nil {
			draft.Temperature = float32(v)
		}
		draft.MaxTokens = atoiOr(maxTokens.GetText(), draft.MaxTokens)
		draft.ReasoningEffort = strings.TrimSpace(reasoning.GetText())
		draft.Thinking = thinkingValue(thinking.Value())
		draft.Project = strings.TrimSpace(project.GetText())
		draft.Location = strings.TrimSpace(location.GetText())

		if draft.Name == "" {
			w.showConfirm("Add model", "A unique model name is required.", nil)
			return
		}
		if draft.Model == "" {
			w.showConfirm("Add model", "A model id is required.", nil)
			return
		}
		if isVertex {
			if draft.Project == "" || draft.Location == "" {
				w.showConfirm("Add model", "Vertex models need a GCP project and location.", nil)
				return
			}
		} else if draft.APIKey == "" {
			w.showConfirm("Add model", "An API key is required.", nil)
			return
		}

		// Resolve a name collision before saving (the user may have edited the
		// auto-generated name into one that now clashes). AddModel re-checks and is
		// the final authority.
		taken := w.takenModelNames()
		base := draft.Name
		for i := 2; taken[draft.Name]; i++ {
			draft.Name = fmt.Sprintf("%s-%d", base, i)
		}

		if err := w.handlers.AddModel(draft); err != nil {
			w.showConfirm("Add model", "Could not add model:\n"+err.Error(), nil)
			return
		}
		w.desktop.RemoveLayer(layer)
		w.refreshModelsAfterSave()
		// Return to a fresh Models… list (showing the new entry) before the
		// follow-up "set as default?" confirm, which then stacks on top of it.
		if onClose != nil {
			onClose()
		}

		saved := draft.Name
		if w.handlers.SetDefaultModel != nil {
			w.showConfirm("Add model", fmt.Sprintf("Added %q.\n\nSet it as the default for new sessions?", saved), func(ok bool) {
				if ok {
					if err := w.handlers.SetDefaultModel(saved); err != nil {
						w.showConfirm("Default model", "Could not set default:\n"+err.Error(), nil)
					}
				}
			})
		} else {
			w.showConfirm("Add model", fmt.Sprintf("Added %q.", saved), nil)
		}
	}

	dialog.Window.AddContent(newButton("Back", tv.Rect{X: 2, Y: height - 3, W: tv.ButtonLabelWidth("Back"), H: 1}, func() {
		w.desktop.RemoveLayer(layer)
		w.showCatalogModelStep(cat, providerID, onClose)
	}))
	dialog.Window.AddContent(newButton("Save", tv.Rect{X: width - 24, Y: height - 3, W: 9, H: 1}, save))
	dialog.Window.AddContent(newButton("Cancel", tv.Rect{X: width - 13, Y: height - 3, W: 10, H: 1}, cancel))

	dialog.Root().OnTypeFn = func(_ *tv.VisualComponent, event tui.TypeEvent) bool {
		if event.Key == tui.KeyEscape {
			cancel()
			return true
		}
		return false
	}

	layer = tv.NewModalLayer("catalog-review", dialog)
	w.desktop.AddLayer(layer)
	dialog.Fit(spec)
	w.desktop.SetFocus(apiKey) // the one field the user must supply
}

// takenModelNames is the set of currently-configured model names, used to keep a
// new catalog entry's name unique.
func (w *Workbench) takenModelNames() map[string]bool {
	taken := map[string]bool{}
	if w.handlers.GetModels == nil {
		return taken
	}
	for _, m := range w.handlers.GetModels() {
		taken[m.Name] = true
	}
	return taken
}

// refreshModelsAfterSave re-fetches the authoritative model list and pushes it
// into the workbench so the sidebar and open session windows pick up the new
// entry immediately (the same pattern as the manual editor's Save, issue #389).
func (w *Workbench) refreshModelsAfterSave() {
	if w.handlers.GetModels != nil {
		if refreshed := w.handlers.GetModels(); len(refreshed) > 0 {
			ptrs := make([]*config.ModelConfig, len(refreshed))
			for i := range refreshed {
				ptrs[i] = &refreshed[i]
			}
			w.SetModels(ptrs)
		}
	}
	w.rebuildMenu()
}
