/*
 * Copyright (c) Cherri
 */

package main

// Inert `nothing` pruning.
//
// addNothing() (parser.go) appends an `is.workflow.actions.nothing` at the end of every control
// flow group. Its purpose is to cut Shortcuts' implicit input chain: an action that does not
// specify its own input receives the previous action's output. A Nothing is therefore only noise
// when the next data-flow action either consumes no input at all, or already specifies its input
// explicitly — otherwise removing it would silently rewire the Shortcut.
//
// The criterion is deliberately fail-closed (unknown next action ⇒ keep the Nothing) and uses the
// identifiers and parameter keys of the verified Python implementation that used to do this as a
// post-processing step (camera-watermark's tools/strip_inert_nothings.py), whose behaviour was
// cross-checked against Apple's own golden shortcuts. In particular a `Nothing` immediately
// followed by a Set Variable with no input must be KEPT: Apple's "WordleBot" shortcut (step 46)
// has exactly that shape, so a Nothing is not unconditionally redundant.
//
// Must run after generateActions() — so the final action graph, parameters included, is known —
// and before generateImportQuestions(): pruning shifts action indices, and import questions
// address actions by index, so those indices are remapped here.

const (
	nothingActionIdent     = "is.workflow.actions.nothing"
	commentActionIdent     = "is.workflow.actions.comment"
	conditionalActionIdent = "is.workflow.actions.conditional"
	menuActionIdent        = "is.workflow.actions.choosefrommenu"
)

// repeatActionIdents are control flow markers as well: their branch markers consume no input.
var repeatActionIdents = map[string]bool{
	"is.workflow.actions.repeat.each":  true,
	"is.workflow.actions.repeat.count": true,
}

// noInputIdents are actions that do not consume the implicit input (sources, pure display, and
// control flow itself).
var noInputIdents = map[string]bool{
	nothingActionIdent:                        true,
	commentActionIdent:                        true,
	"is.workflow.actions.exit":                true,
	"is.workflow.actions.alert":               true,
	"is.workflow.actions.notification":        true,
	"is.workflow.actions.dictionary":          true,
	"is.workflow.actions.list":                true,
	"is.workflow.actions.downloadurl":         true,
	"is.workflow.actions.documentpicker.open": true,
	"is.workflow.actions.selectphoto":         true,
	"is.workflow.actions.file.select":         true,
	"is.workflow.actions.takephoto":           true,
	"is.workflow.actions.gettext":             true,
	"is.workflow.actions.ask":                 true,
	"is.workflow.actions.gettypeaction":       true,
	"is.workflow.actions.setclipboard":        true,
}

// inputKeyIdentifiers maps an action that consumes the implicit input to the parameter key it
// would have to be given explicitly for a preceding Nothing to be inert.
var inputKeyIdentifiers = map[string]string{
	"is.workflow.actions.setvariable":          "WFInput",
	"is.workflow.actions.text.replace":         "WFInput",
	"is.workflow.actions.getvalueforkey":       "WFInput",
	"is.workflow.actions.detect.images":        "WFInput",
	"is.workflow.actions.detect.text":          "WFInput",
	"is.workflow.actions.detect.dictionary":    "WFInput",
	"is.workflow.actions.detect.link":          "WFInput",
	"is.workflow.actions.properties.images":    "WFInput",
	"is.workflow.actions.properties.locations": "WFInput",
	"is.workflow.actions.base64encode":         "WFInput",
	"is.workflow.actions.base64decode":         "WFInput",
	"is.workflow.actions.count":                "Input",
	"is.workflow.actions.number":               "WFNumberActionNumber",
	"is.workflow.actions.calculateexpression":  "Input",
	"is.workflow.actions.choosefromlist":       "WFInput",
	"is.workflow.actions.file.delete":          "WFInput",
	"is.workflow.actions.documentpicker.save":  "WFInput",
	"is.workflow.actions.savetocameraroll":     "WFInput",
	"is.workflow.actions.image.combine":        "WFInput",
	"is.workflow.actions.image.resize":         "WFImage",
	"is.workflow.actions.image.convert":        "WFInput",
	"is.workflow.actions.overlayimageonimage":  "WFInput",
	"is.workflow.actions.format.date":          "WFDate",
	"is.workflow.actions.url":                  "WFURLActionURL",
	"is.workflow.actions.openurl":              "WFInput",
	"is.workflow.actions.splitpdf":             "WFInput",
	"is.workflow.actions.unzip":                "WFArchive",
	"is.workflow.actions.makeimagefrompdfpage": "WFInput",
	"is.workflow.actions.makezip":              "WFInput",
}

// pruneInertNothings drops every provably inert nothing action from the generated action graph and
// remaps import question indices accordingly. Returns the number of actions removed.
func pruneInertNothings() int {
	var actions = shortcut.WFWorkflowActions
	if len(actions) == 0 {
		return 0
	}

	var removed = make([]bool, len(actions))
	for i := range actions {
		if actions[i].WFWorkflowActionIdentifier == nothingActionIdent && nothingIsInert(actions, i) {
			removed[i] = true
		}
	}

	// removeActions() rebuilds the action list and remaps import question indices.
	return removeActions(removed)
}

// nothingIsInert reports whether the nothing action at index i can be removed without changing
// what the Shortcut does.
func nothingIsInert(actions []ShortcutAction, i int) bool {
	// Comments are display-only and may sit between the Nothing and the next data-flow action.
	var j = i + 1
	for j < len(actions) && actions[j].WFWorkflowActionIdentifier == commentActionIdent {
		j++
	}
	if j >= len(actions) {
		return true // trailing Nothing: no action follows that could receive input
	}

	var ident = actions[j].WFWorkflowActionIdentifier
	var params = actions[j].WFWorkflowActionParameters

	if isControlFlowMarker(ident) {
		if controlFlowMode(params) != 0 {
			return true // end/else/branch marker: consumes no input
		}
		return params["WFInput"] != nil // group start: inert only if it was given its own input
	}

	if noInputIdents[ident] {
		return true
	}

	var key, known = inputKeyIdentifiers[ident]
	if !known {
		return false // unknown action: fail closed and keep the Nothing
	}
	return params[key] != nil
}

func isControlFlowMarker(ident string) bool {
	return ident == conditionalActionIdent || ident == menuActionIdent || repeatActionIdents[ident]
}

// controlFlowMode reads WFControlFlowMode as an integer regardless of the concrete type it was
// stored with. The generator stores it as the uint64 constants startStatement/statementPart/
// endStatement; an absent value means a group start (mode 0), matching the reference
// implementation this pruning was ported from.
func controlFlowMode(params map[string]any) int {
	switch mode := params["WFControlFlowMode"].(type) {
	case uint64:
		return int(mode)
	case uint:
		return int(mode)
	case int:
		return mode
	case int64:
		return int(mode)
	case float64:
		return int(mode)
	}
	return 0
}
