package ui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gogent/internal/config"
	"gogent/internal/model"
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
// form. Every aspect models.dev can supply is surfaced with consistent provenance
// (issue #542): read-only facts (API type, context window, cost, capabilities,
// docs) render as "(from catalog)" labels; catalog-seeded editable fields (max
// tokens, reasoning effort) show their source as a hint; the derive-base Endpoint
// shows the resolved/derived base read-only while staying overridable; and the
// provider credential env var is carried forward. Save creates a NEW entry via
// handlers.AddModel and refreshes the app.
func (w *Workbench) showCatalogReviewStep(cat modelsdev.Catalog, providerID, modelID string, onClose func()) {
	p := cat[providerID]
	cm := p.Models[modelID]

	// Build the connection draft (credentials/api_type/endpoint) and the model draft
	// referencing it. The catalog add creates BOTH: a connection (reused if one with
	// the same name already exists) plus the model that points at it.
	connDraft := modelsdev.ToConnection(p)
	connDraft.Name = w.uniqueConnectionName(connDraft.Name)
	draft := modelsdev.ToModelConfig(providerID, connDraft.Name, cm)
	draft.Name = modelsdev.UniqueName(providerID, modelID, w.takenModelNames())
	apiType := model.StringToAPIType(connDraft.APIType)
	isVertex := strings.HasPrefix(connDraft.APIType, "vertex")

	// Endpoint provenance: a provider that derives its own base (the registry's
	// derivesBase flag — the SAME source of truth ToModelConfig leaves Endpoint blank
	// for) shows the resolved/derived base read-only while the box stays blank, so the
	// persisted endpoint stays empty unless the user overrides it. Branching on
	// DerivesBase rather than on ResolvedBaseURL's outputs keeps the dialog aligned
	// with routing even for a future derive-base provider whose base ResolvedBaseURL
	// cannot name. OpenAI-compatible gateways (derivesBase=false) keep the editable
	// box prefilled with the catalog's p.API.
	derivesBase := model.DerivesBase(apiType)

	const labelW = 16
	const boxX = 2 + labelW
	spec := tv.DialogSpec{MinW: 76, MinH: 21, MaxH: 21, PreferredW: 84}
	x, y, width, height := w.dialogRect(spec)
	boxW := width - boxX - 3

	dialog := tv.NewDialog(fmt.Sprintf("Add model — %s", modelLabel(cm)), x, y, width, height)
	applyWindowShadow(dialog.Window)
	dialog.Window.ShowClose = false

	// field adds a full-width labelled editable TextBox.
	field := func(text string, row int) *tv.TextBox {
		dialog.Window.AddContent(dialogLabel(text, tv.Rect{X: 2, Y: row, W: labelW, H: 1}))
		box := tv.NewTextBox("", tv.Rect{X: boxX, Y: row, W: boxW, H: 1})
		dialog.Window.AddContent(box)
		return box
	}
	// fieldHint adds a labelled editable TextBox of width bw plus a read-only hint to
	// its right — used to attach catalog provenance without crowding out the input.
	fieldHint := func(text string, row, bw int, hint string) *tv.TextBox {
		dialog.Window.AddContent(dialogLabel(text, tv.Rect{X: 2, Y: row, W: labelW, H: 1}))
		box := tv.NewTextBox("", tv.Rect{X: boxX, Y: row, W: bw, H: 1})
		dialog.Window.AddContent(box)
		if hint != "" {
			hx := boxX + bw + 1
			if hw := width - 3 - hx; hw > 0 {
				dialog.Window.AddContent(dialogLabel(hint, tv.Rect{X: hx, Y: row, W: hw, H: 1}))
			}
		}
		return box
	}
	// infoRow adds a full-width read-only catalog-fact line.
	infoRow := func(text string, row int) {
		dialog.Window.AddContent(dialogLabel(text, tv.Rect{X: 2, Y: row, W: width - 4, H: 1}))
	}

	row := 1
	name := field("Name:", row)
	name.SetText(draft.Name)
	row++

	// API type is read-only ("from catalog"): turbotui's Select has no disabled
	// state, so render the derived type as a static label rather than a control the
	// user can't meaningfully change. The value travels in the draft.
	dialog.Window.AddContent(dialogLabel("API type:", tv.Rect{X: 2, Y: row, W: labelW, H: 1}))
	dialog.Window.AddContent(dialogLabel(connDraft.APIType+"  (from catalog)", tv.Rect{X: boxX, Y: row, W: boxW, H: 1}))
	row++

	display := field("Display name:", row)
	display.SetText(draft.DisplayName)
	row++

	// Endpoint: empty box + read-only derived hint for derive-base providers (the
	// blank persists unless the user types an override); editable box prefilled with
	// p.API for OpenAI-compatible gateways.
	var endpoint *tv.TextBox
	if derivesBase {
		base, fromProjectLocation := model.ResolvedBaseURL(apiType)
		var hint string
		switch {
		case fromProjectLocation:
			hint = "(derived from Project + Location)"
		case base != "":
			hint = fmt.Sprintf("(derived: %s)", base)
		default:
			hint = "(derived by the adapter)" // derive-base type whose base we can't name
		}
		endpoint = fieldHint("Endpoint:", row, 8, hint) // box left blank on purpose
	} else {
		endpoint = field("Endpoint:", row)
		endpoint.SetText(connDraft.Endpoint)
	}
	row++

	// Model id: a plain, editable text field pre-filled with the catalog
	// selection. Unlike the manual editor there is deliberately NO "Scan" button
	// here: the id is already the model the user just picked from the catalog, so
	// scanning is redundant — and a draft scan cannot work in remote mode anyway,
	// where ScanModels is keyed by a SAVED model name (the unsaved draft would
	// 404). The user can still hand-edit the id.
	modelIDBox := field("Model id:", row)
	modelIDBox.SetText(draft.Model)
	row++

	// Credential: Vertex authenticates with ADC (project/location, no key); every
	// other provider needs an API key, with the provider's credential env var
	// carried forward from the picker step as a hint.
	var apiKey, project, location *tv.TextBox
	if isVertex {
		project = field("Project:", row)
		project.SetText(connDraft.Project)
		row++
		location = field("Location:", row)
		location.SetText(connDraft.Location)
		row++
	} else if len(p.Env) > 0 {
		apiKey = fieldHint("API key:", row, boxW-28, "(env: "+strings.Join(p.Env, ", ")+")")
		row++
	} else {
		apiKey = field("API key:", row)
		row++
	}

	temp := field("Temperature:", row)
	temp.SetText(strconv.FormatFloat(float64(draft.Temperature), 'g', -1, 32))
	row++

	// The catalog output limit lives on Caps.MaxOutput now; seed the editable
	// per-request cap from it (the user's MaxTokens override defaults to it).
	outCap := draft.MaxTokens
	if outCap == 0 {
		outCap = draft.Caps.MaxOutput
	}
	maxHint := ""
	if outCap > 0 {
		maxHint = "(from catalog output limit)"
	}
	maxTokens := fieldHint("Max tokens:", row, 12, maxHint)
	maxTokens.SetText(strconv.Itoa(outCap))
	row++

	// Reasoning effort: a Select constrained to the catalog's valid set when the
	// model exposes one (with a leading "(none)" that clears the effort, preserving
	// the opt-out the old free-text box allowed); the free-text fallback otherwise.
	var effortSelect *tv.Select
	var effortBox *tv.TextBox
	if len(draft.Caps.EffortOptions) > 0 {
		opts := append([]string{effortNone}, draft.Caps.EffortOptions...)
		dialog.Window.AddContent(dialogLabel("Reasoning:", tv.Rect{X: 2, Y: row, W: labelW, H: 1}))
		effortSelect = newSelect(w.desktop, opts, tv.Rect{X: boxX, Y: row, W: boxW - 16, H: 1})
		effortSelect.SetSelected(effortIndex(opts, draft.ReasoningEffort))
		dialog.Window.AddContent(effortSelect)
		dialog.Window.AddContent(dialogLabel("(from catalog)", tv.Rect{X: boxX + boxW - 15, Y: row, W: 14, H: 1}))
	} else {
		effortBox = field("Reasoning:", row)
		effortBox.SetText(draft.ReasoningEffort)
	}
	row++

	// Thinking: shown for every model but annotated with whether it actually does
	// anything — the provider must emit `thinking` AND the model must advertise a
	// toggle. Kept editable (no lockout) and normal-coloured; relevance is signalled
	// by the hint, never by greying (grey means "inert" elsewhere in the app).
	thinkingHint := "(no effect for this model)"
	if model.SupportsThinking(apiType) && modelsdev.HasThinkingToggle(cm) {
		thinkingHint = "(supported)"
	}
	dialog.Window.AddContent(dialogLabel("Thinking:", tv.Rect{X: 2, Y: row, W: labelW, H: 1}))
	thinking := newSelect(w.desktop, []string{"default", "on", "off"}, tv.Rect{X: boxX, Y: row, W: 12, H: 1})
	thinking.SetSelected(thinkingIndex(draft.Thinking))
	dialog.Window.AddContent(thinking)
	if hx := boxX + 13; width-3-hx > 0 {
		dialog.Window.AddContent(dialogLabel(thinkingHint, tv.Rect{X: hx, Y: row, W: width - 3 - hx, H: 1}))
	}
	row++

	// Read-only catalog facts: context window + pricing on one line, then
	// capabilities, then docs (when present).
	facts := make([]string, 0, 2)
	if cm.Limit.Context > 0 {
		facts = append(facts, "Context "+tokensShort(cm.Limit.Context))
	}
	facts = append(facts, "Cost "+modelsdev.CostSummary(cm))
	infoRow(strings.Join(facts, " · ")+"  (from catalog)", row)
	row++

	if caps := modelsdev.CapabilityLabels(cm); len(caps) > 0 {
		infoRow("Capabilities: "+strings.Join(caps, " · "), row)
		row++
	}
	if doc := strings.TrimSpace(p.Doc); doc != "" {
		infoRow("Docs: "+doc, row) // Docs is the last content row; the button row is pinned to height-3
	}

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
		connDraft.Endpoint = strings.TrimSpace(endpoint.GetText())
		draft.Model = strings.TrimSpace(modelIDBox.GetText())
		if apiKey != nil {
			connDraft.APIKey = apiKey.GetText()
		}
		if v, err := strconv.ParseFloat(temp.GetText(), 32); err == nil {
			draft.Temperature = float32(v)
		}
		draft.MaxTokens = atoiOr(maxTokens.GetText(), draft.MaxTokens)
		switch {
		case effortSelect != nil:
			if v := effortSelect.Value(); v == effortNone {
				draft.ReasoningEffort = ""
			} else {
				draft.ReasoningEffort = strings.TrimSpace(v)
			}
		case effortBox != nil:
			draft.ReasoningEffort = strings.TrimSpace(effortBox.GetText())
		}
		draft.Thinking = thinkingValue(thinking.Value())
		if project != nil {
			connDraft.Project = strings.TrimSpace(project.GetText())
		}
		if location != nil {
			connDraft.Location = strings.TrimSpace(location.GetText())
		}

		if draft.Name == "" {
			w.showConfirm("Add model", "A unique model name is required.", nil)
			return
		}
		if draft.Model == "" {
			w.showConfirm("Add model", "A model id is required.", nil)
			return
		}
		if isVertex {
			if connDraft.Project == "" || connDraft.Location == "" {
				w.showConfirm("Add model", "Vertex models need a GCP project and location.", nil)
				return
			}
		} else if connDraft.APIKey == "" {
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

		// Create the connection first (idempotent: a pre-existing same-name connection
		// is reused), then the model that references it.
		if w.handlers.AddConnection != nil {
			if err := w.handlers.AddConnection(connDraft); err != nil &&
				!strings.Contains(err.Error(), "already exists") {
				w.showConfirm("Add model", "Could not add connection:\n"+err.Error(), nil)
				return
			}
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
	// Focus the one field the user must supply: the API key, or — for Vertex (ADC,
	// no key) — the GCP project.
	if isVertex {
		w.desktop.SetFocus(project)
	} else {
		w.desktop.SetFocus(apiKey)
	}
}

// effortNone is the leading reasoning-effort Select option that clears
// ReasoningEffort (opting the model out of reasoning), preserving the opt-out the
// pre-#542 free-text field allowed while still constraining the rest of the choices
// to the catalog's valid set.
const effortNone = "(none)"

// effortIndex returns the index of want in opts, or 0 (the "(none)" entry) when it
// is absent, so the effort Select opens on the catalog default.
func effortIndex(opts []string, want string) int {
	for i, o := range opts {
		if o == want {
			return i
		}
	}
	return 0
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

// connectionNames returns the configured provider-connection names.
func (w *Workbench) connectionNames() []string {
	if w.handlers.GetConnections == nil {
		return nil
	}
	conns := w.handlers.GetConnections()
	names := make([]string, 0, len(conns))
	for _, c := range conns {
		names = append(names, c.Name)
	}
	return names
}

// uniqueConnectionName suffixes -2/-3/… when base collides with an existing
// connection name, so a catalog add can synthesize a fresh connection.
func (w *Workbench) uniqueConnectionName(base string) string {
	if strings.TrimSpace(base) == "" {
		base = "connection"
	}
	taken := map[string]bool{}
	for _, n := range w.connectionNames() {
		taken[n] = true
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !taken[cand] {
			return cand
		}
	}
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
