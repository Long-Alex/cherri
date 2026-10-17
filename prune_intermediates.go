/*
 * Copyright (c) Cherri
 */

package main

// Generation-time pruning of pass-through intermediates.
//
// The parser emits one Set Variable per mutable assignment and one `detect.text` per
// getText() call. A large share of those are pure pass-throughs that only exist because
// the language has no expression for "use this reference here":
//
//	P4 (pruneAliasSetVariables) — a Set Variable whose only non-empty write is a plain
//	   reference (another variable, a magic variable, or an action output) and whose reads
//	   all happen after that write. Each read can point at the reference directly.
//
//	P5 (pruneTextConversions) — a `detect.text` whose result is consumed exactly once, by
//	   an action that coerces its own input to text anyway (Count with "Characters", a
//	   dictionary key, a file path, or a Set Variable that is itself a pure pass-through).
//
// Until 2026-10-17 both reductions were done on the compiled plist by the camera-watermark
// project's tools/simplify_intermediate_steps.py — a post-processing step whose rules were
// keyed to that project's variable names. These passes implement the same reduction as
// general, name-agnostic rules so the compiler itself emits the smaller graph (and the
// post-processing step is no longer needed).
//
// Both passes are deliberately fail-closed: anything the pass cannot prove is left alone.
// They must run after generateActions() — a pass has to see the whole graph (writers,
// readers, control flow scopes) before it can decide — and before generateImportQuestions():
// pruning shifts action indices, which import questions address by index.
//
// The "is this safe?" rule for P4 is dominance:
//   - an ActionOutput source is only inlined when the alias write and every read sit in the
//     same control-flow scope, so the producing action has certainly run by the time the
//     read happens (action outputs of actions that never ran are empty, so a read from a
//     different scope would silently change the value);
//   - a magic variable (Repeat Item, Shortcut Input, …) is always in scope, so no scope
//     check applies;
//   - a user-variable source is inlined only when that variable provably holds the same
//     value at every read: all of its writes are either empty initializers, or happen in
//     the same control-flow block as the alias write and before it. That is the "snapshot"
//     shape the post-processing tool used to special-case (`@latitudeText = getText(@latitude)`
//     written right after `@latitude = …` in the same `if`).

import "reflect"

const (
	setVariableActionIdent = "is.workflow.actions.setvariable"
	detectTextActionIdent  = "is.workflow.actions.detect.text"
	countActionIdent       = "is.workflow.actions.count"
	dictionaryValueIdent   = "is.workflow.actions.getvalueforkey"
	openFileActionIdent    = "is.workflow.actions.documentpicker.open"
)

// selfCoercingConsumerKeys maps a consumer action to the parameter keys whose value the
// consumer converts to text on its own, making a preceding `detect.text` redundant.
//   - Count coerces its input to text for the "Characters" count type.
//   - A dictionary key is text by definition.
//   - A file path is text by definition.
//   - A Set Variable that stores exactly this reference is a pure pass-through (it becomes
//     an alias, which pruneAliasSetVariables() may then inline as well).
var selfCoercingConsumerKeys = map[string]map[string]bool{
	countActionIdent:       {"Input": true, "WFInput": true},
	dictionaryValueIdent:   {"WFDictionaryKey": true},
	openFileActionIdent:    {"WFGetFilePath": true},
	setVariableActionIdent: {"WFInput": true},
}

// magicVariableNames are Shortcuts' built-in variables: always in scope, no producer to
// dominate. (cherri stores the display name, e.g. "Repeat Item", in the reference.)
var magicVariableNames = map[string]bool{
	"Repeat Item":         true,
	"Repeat Index":        true,
	"Shortcut Input":      true,
	"Current Date":        true,
	"Clipboard":           true,
	"Device":              true,
	"Ask for Input":       true,
	"Ask Each Time":       true,
	"Quick Look Result":   true,
	"Selected Text":       true,
	"Text from Clipboard": true,
}

// refSite is one reference occurrence inside a generated action.
type refSite struct {
	idx int    // index of the action holding the reference
	key string // top-level parameter the reference was found under
	val Value  // the reference itself
}

// pruneIntermediates removes provably redundant pass-through actions and remaps import
// question indices. Returns the number of actions removed.
func pruneIntermediates() int {
	var total int
	// Inlining a text conversion can turn a Set Variable into an alias (and vice versa),
	// so the two passes are repeated until neither of them finds anything.
	for i := 0; i < 8; i++ {
		var removed = pruneTextConversions() + pruneAliasSetVariables()
		if removed == 0 {
			break
		}
		total += removed
	}
	return total
}

// ── P5: `detect.text` used exactly once by a self-coercing consumer ────────────────────

// pruneTextConversions drops every `detect.text` whose output is referenced exactly once and
// whose only consumer converts its input to text itself. Returns the number removed.
func pruneTextConversions() int {
	var actions = shortcut.WFWorkflowActions
	if len(actions) == 0 {
		return 0
	}

	var sites = make(map[string][]refSite)
	for i := range actions {
		for _, ref := range refsIn(actions[i].WFWorkflowActionParameters) {
			if ref.val.Type == "ActionOutput" && ref.val.OutputUUID != "" {
				sites[ref.val.OutputUUID] = append(sites[ref.val.OutputUUID], refSite{idx: i, key: ref.key, val: ref.val})
			}
		}
	}

	var removed = make([]bool, len(actions))
	var count int
	for i := range actions {
		if actions[i].WFWorkflowActionIdentifier != detectTextActionIdent {
			continue
		}
		var uuid, _ = actions[i].WFWorkflowActionParameters["UUID"].(string)
		if uuid == "" {
			continue
		}
		var found = sites[uuid]
		if len(found) != 1 {
			continue // no consumer, or more than one: not provable
		}
		var site = found[0]
		if site.idx == i {
			continue
		}
		var consumer = actions[site.idx]
		var allowed, known = selfCoercingConsumerKeys[consumer.WFWorkflowActionIdentifier]
		if !known || !allowed[site.key] {
			continue // the consumer may depend on the value actually being text
		}
		var source, ok = singleRef(actions[i].WFWorkflowActionParameters["WFInput"])
		if !ok {
			continue // input is not a plain reference (literal / multi-part text): leave it
		}
		if consumer.WFWorkflowActionIdentifier == setVariableActionIdent {
			// Only when the Set Variable's whole input is this reference; storing a text
			// token that merely contains it would change the stored value.
			if _, isRef := singleRef(consumer.WFWorkflowActionParameters["WFInput"]); !isRef {
				continue
			}
		}

		var updated, replaced = replaceParams(consumer.WFWorkflowActionParameters,
			func(v Value) bool { return v.Type == "ActionOutput" && v.OutputUUID == uuid }, source)
		if replaced != 1 {
			continue // fail closed: the reference showed up somewhere unexpected
		}
		consumer.WFWorkflowActionParameters = updated
		shortcut.WFWorkflowActions[site.idx] = consumer
		removed[i] = true
		count++
	}

	if count > 0 {
		removeActions(removed)
	}
	return count
}

// ── P4: Set Variable that is a pure alias of another reference ─────────────────────────

// pruneAliasSetVariables drops every Set Variable whose value is only ever a single
// reference and whose reads all happen after that write, rewiring the reads to the
// referenced value. Returns the number removed.
func pruneAliasSetVariables() int {
	var actions = shortcut.WFWorkflowActions
	if len(actions) == 0 {
		return 0
	}

	var writes = make(map[string][]int)
	var reads = make(map[string][]refSite)
	for i := range actions {
		var params = actions[i].WFWorkflowActionParameters
		for _, ref := range refsIn(params) {
			if ref.val.Type == "Variable" && ref.val.VariableName != "" {
				reads[ref.val.VariableName] = append(reads[ref.val.VariableName], refSite{idx: i, key: ref.key, val: ref.val})
			}
		}
		if actions[i].WFWorkflowActionIdentifier != setVariableActionIdent {
			continue
		}
		var name, _ = params["WFVariableName"].(string)
		if name != "" {
			writes[name] = append(writes[name], i)
		}
	}

	var paths = blockPaths(actions)
	var removed = make([]bool, len(actions))
	var count int

	for name, sites := range writes {
		var alias = -1
		var inits []int
		var usable = true
		for _, w := range sites {
			if source, ok := singleRef(actions[w].WFWorkflowActionParameters["WFInput"]); ok &&
				(source.Type == "ActionOutput" || source.Type == "Variable") {
				if alias >= 0 {
					usable = false // two non-empty writes: not an alias
					break
				}
				alias = w
				continue
			}
			if isEmptyInput(actions[w].WFWorkflowActionParameters["WFInput"]) {
				inits = append(inits, w)
				continue
			}
			usable = false // some other value is written
			break
		}
		if !usable || alias < 0 {
			continue
		}
		var source, _ = singleRef(actions[alias].WFWorkflowActionParameters["WFInput"])

		// Every read must happen after the alias write, and every initializer before it.
		var consumers = reads[name]
		if len(consumers) == 0 {
			continue // never read: not this pass's business
		}
		var ok = true
		for _, init := range inits {
			if init > alias {
				ok = false
				break
			}
		}
		for _, read := range consumers {
			if read.idx <= alias || (inits != nil && read.idx < alias) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if !aliasSourceIsSafe(source, alias, consumers, paths, writes, actions) {
			continue
		}

		// Rewire every read to the source reference, then drop the alias and its initializers.
		var rewriteFailed bool
		for _, read := range consumers {
			var holder = shortcut.WFWorkflowActions[read.idx]
			var updated, replaced = replaceParams(holder.WFWorkflowActionParameters,
				func(v Value) bool { return v.Type == "Variable" && v.VariableName == name }, source)
			if replaced == 0 {
				rewriteFailed = true
				break
			}
			holder.WFWorkflowActionParameters = updated
			shortcut.WFWorkflowActions[read.idx] = holder
		}
		if rewriteFailed || referencesVariable(shortcut.WFWorkflowActions, name) {
			continue // fail closed: do not delete a variable that is still referenced
		}
		removed[alias] = true
		for _, init := range inits {
			removed[init] = true
		}
		count += 1 + len(inits)
	}

	if count > 0 {
		removeActions(removed)
	}
	return count
}

// aliasSourceIsSafe reports whether reads of the aliased variable can be pointed straight at
// source without changing the value they observe. See the file header for the reasoning.
func aliasSourceIsSafe(source Value, alias int, consumers []refSite, paths [][]int,
	writes map[string][]int, actions []ShortcutAction) bool {
	switch source.Type {
	case "Variable":
		if magicVariableNames[source.VariableName] {
			return true
		}
		// Snapshot: the source variable must not change between the alias write and the
		// reads. Every write of it is either an empty initializer, or happens in the same
		// block as the alias write and before it.
		var sourceWrites = writes[source.VariableName]
		if len(sourceWrites) == 0 {
			return false // unknown provenance: fail closed
		}
		for _, w := range sourceWrites {
			if suppressEmptyInit(actions[w]) {
				continue
			}
			if !samePath(paths[w], paths[alias]) || w > alias {
				return false
			}
		}
		return true
	case "ActionOutput":
		// Same control-flow scope everywhere ⇒ the producer has run before every read.
		for _, read := range consumers {
			if !samePath(paths[read.idx], paths[alias]) {
				return false
			}
		}
		return true
	}
	return false
}

func suppressEmptyInit(action ShortcutAction) bool {
	return isEmptyInput(action.WFWorkflowActionParameters["WFInput"])
}

// blockPaths returns, for every action, the control flow blocks enclosing it (identified by
// the index of the block's start marker).
func blockPaths(actions []ShortcutAction) [][]int {
	var paths = make([][]int, len(actions))
	var stack []int
	for i := range actions {
		var ident = actions[i].WFWorkflowActionIdentifier
		if isControlFlowMarker(ident) {
			var mode = controlFlowMode(actions[i].WFWorkflowActionParameters)
			if mode == 0 {
				paths[i] = append([]int(nil), stack...)
				stack = append(stack, i)
				continue
			}
			if mode == 2 && len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			paths[i] = append([]int(nil), stack...)
			continue
		}
		paths[i] = append([]int(nil), stack...)
	}
	return paths
}

func samePath(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── plist value helpers ────────────────────────────────────────────────────────────────

// singleRef returns the reference a parameter value is exactly, or false when the value is
// anything else (a literal, a text token, a dictionary, an empty value).
func singleRef(value any) (Value, bool) {
	switch typed := value.(type) {
	case WFTextTokenAttachment:
		if isReference(typed.Value) {
			return typed.Value, true
		}
	case *WFTextTokenAttachment:
		if typed != nil && isReference(typed.Value) {
			return typed.Value, true
		}
	}
	return Value{}, false
}

func isReference(value Value) bool {
	if value.Type == "ActionOutput" {
		return value.OutputUUID != ""
	}
	return value.Type == "Variable" && value.VariableName != ""
}

// isEmptyInput reports whether a Set Variable's WFInput carries an empty literal (or nothing
// at all) — i.e. the action only initializes the variable.
func isEmptyInput(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case WFTextTokenString:
		return len(typed.Value.AttachmentsByRange) == 0 && typed.Value.String == ""
	case *WFTextTokenString:
		return typed == nil || (len(typed.Value.AttachmentsByRange) == 0 && typed.Value.String == "")
	case map[string]any:
		// The literal inlining path stores a bare string as a plain map.
		var inner, _ = typed["Value"].(map[string]any)
		if inner == nil {
			return false
		}
		var text, _ = inner["string"].(string)
		var attachments, _ = inner["attachmentsByRange"].(map[string]any)
		return text == "" && len(attachments) == 0
	}
	return false
}

// refsIn returns every reference held by an action's parameters, together with the name of
// the top-level parameter it was found under.
func refsIn(params map[string]any) []refSite {
	var found []refSite
	for key, value := range params {
		walkValues(reflect.ValueOf(value), func(ref Value) {
			if isReference(ref) {
				found = append(found, refSite{key: key, val: ref})
			}
		})
	}
	return found
}

// walkValues visits every Value struct reachable from a plist value.
func walkValues(value reflect.Value, visit func(Value)) {
	if !value.IsValid() {
		return
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Ptr:
		if value.IsNil() {
			return
		}
		walkValues(value.Elem(), visit)
	case reflect.Struct:
		if value.Type() == valueStructType {
			var v = value.Interface().(Value)
			visit(v)
			// A Value may itself nest further values (e.g. inside a dictionary item).
			for i := 0; i < value.NumField(); i++ {
				if value.Type().Field(i).PkgPath != "" {
					continue
				}
				walkValues(value.Field(i), visit)
			}
			return
		}
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).PkgPath != "" {
				continue
			}
			walkValues(value.Field(i), visit)
		}
	case reflect.Map:
		var iter = value.MapRange()
		for iter.Next() {
			walkValues(iter.Value(), visit)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			walkValues(value.Index(i), visit)
		}
	}
}

var valueStructType = reflect.TypeOf(Value{})

// replaceParams is replaceRefs() for an action's parameter dictionary.
func replaceParams(params map[string]any, match func(Value) bool, repl Value) (map[string]any, int) {
	var out, replaced = replaceRefs(params, match, repl)
	if typed, ok := out.(map[string]any); ok {
		return typed, replaced
	}
	return params, 0
}

// referencesVariable reports whether any action still reads the given variable.
func referencesVariable(actions []ShortcutAction, name string) bool {
	for i := range actions {
		for _, ref := range refsIn(actions[i].WFWorkflowActionParameters) {
			if ref.val.Type == "Variable" && ref.val.VariableName == name {
				return true
			}
		}
	}
	return false
}

// replaceRefs returns a copy of a plist value with every reference for which match reports
// true replaced by repl, together with the number of replacements.
func replaceRefs(value any, match func(Value) bool, repl Value) (any, int) {
	var replacements int
	var out = mapValues(reflect.ValueOf(value), func(ref Value) Value {
		if match(ref) {
			replacements++
			return repl
		}
		return ref
	})
	return out.Interface(), replacements
}

// mapValues returns a copy of a plist value with every Value struct passed through f.
func mapValues(value reflect.Value, f func(Value) Value) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return value
		}
		var inner = mapValues(value.Elem(), f)
		var out = reflect.New(value.Type()).Elem()
		out.Set(inner)
		return out
	case reflect.Ptr:
		if value.IsNil() {
			return value
		}
		var inner = mapValues(value.Elem(), f)
		var out = reflect.New(value.Type().Elem())
		out.Elem().Set(inner)
		return out
	case reflect.Struct:
		if value.Type() == valueStructType {
			var replaced = reflect.ValueOf(f(value.Interface().(Value)))
			var out = reflect.New(valueStructType).Elem()
			out.Set(replaced)
			for i := 0; i < out.NumField(); i++ {
				if valueStructType.Field(i).PkgPath != "" || !out.Field(i).CanSet() {
					continue
				}
				out.Field(i).Set(mapValues(out.Field(i), f))
			}
			return out
		}
		var out = reflect.New(value.Type()).Elem()
		out.Set(value)
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).PkgPath != "" || !out.Field(i).CanSet() {
				continue
			}
			out.Field(i).Set(mapValues(value.Field(i), f))
		}
		return out
	case reflect.Map:
		var out = reflect.MakeMapWithSize(value.Type(), value.Len())
		var iter = value.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), mapValues(iter.Value(), f))
		}
		return out
	case reflect.Slice:
		var out = reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(mapValues(value.Index(i), f))
		}
		return out
	}
	return value
}

// removeActions drops the marked actions and shifts import question indices down by the
// number of actions pruned in front of them. Returns the number of actions removed.
func removeActions(removed []bool) int {
	var actions = shortcut.WFWorkflowActions
	var kept = make([]ShortcutAction, 0, len(actions))
	for i := range actions {
		if removed[i] {
			continue
		}
		kept = append(kept, actions[i])
	}
	var count = len(actions) - len(kept)
	if count == 0 {
		return 0
	}
	shortcut.WFWorkflowActions = kept
	for _, q := range questions {
		var shift int
		for i := 0; i < q.actionIndex && i < len(removed); i++ {
			if removed[i] {
				shift++
			}
		}
		q.actionIndex -= shift
	}
	return count
}
