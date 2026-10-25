package taskstate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Entry kinds of an EntryRef: the Task State entries whose text a run or the user writes.
const (
	EntryNote      = "note"
	EntryReference = "ref"
	EntryUnit      = "unit"
	EntryTask      = "task"
)

// EntryRef is the locator of one version of a Task State entry, "ts:<kind>:<id>@sha256:<hex>", the
// digest being that of the entry's stored JSON. Provenance edges (ADR-0018) name entries by it: a
// different version is a different ref, and the ref holds no text.
func EntryRef(kind, id string, entry any) string {
	raw, err := json.Marshal(entry)
	if err != nil {
		// Every entry type is plain data; this cannot fail for a state that encodes.
		panic("task state entry is not serializable: " + err.Error())
	}
	sum := sha256.Sum256(raw)
	return "ts:" + kind + ":" + id + "@sha256:" + hex.EncodeToString(sum[:])
}

// EntryChange is an entry whose version differs between two states: new, or rewritten.
type EntryChange struct {
	Ref string
	// From is the entry's From (a user rewrite of agent text), "" otherwise.
	From string
}

// ChangedEntries lists the entries of after that before does not hold in the same version, in a
// stable order. Removed entries are not listed: a removal derives nothing.
func ChangedEntries(before, after State) []EntryChange {
	had := map[string]bool{}
	for _, e := range before.entries() {
		had[e.Ref] = true
	}
	var out []EntryChange
	for _, e := range after.entries() {
		if !had[e.Ref] {
			out = append(out, e)
		}
	}
	return out
}

func (s State) entries() []EntryChange {
	var out []EntryChange
	for _, n := range s.Notes {
		out = append(out, EntryChange{Ref: EntryRef(EntryNote, n.ID, n), From: n.From})
	}
	for _, r := range s.References {
		out = append(out, EntryChange{Ref: EntryRef(EntryReference, r.ID, r), From: r.From})
	}
	if s.WorkUnits != nil {
		ids := make([]string, 0, len(s.WorkUnits.Items))
		for id := range s.WorkUnits.Items {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			out = append(out, EntryChange{Ref: EntryRef(EntryUnit, id, s.WorkUnits.Items[id])})
		}
	}
	for _, t := range s.Tasks {
		out = append(out, EntryChange{Ref: EntryRef(EntryTask, t.ID, t)})
	}
	return out
}

// ChildStart is an execution task that gained its child session between two states.
type ChildStart struct {
	SessionID string
	// Proposal is the entry ref of the task as before held it (a proposal the user started), ""
	// when before did not hold the task (a run started it directly).
	Proposal string
}

// StartedChildren lists the tasks of after whose child session before did not record.
func StartedChildren(before, after State) []ChildStart {
	var out []ChildStart
	for _, t := range after.Tasks {
		if t.SessionID == "" {
			continue
		}
		prior, ok := before.FindTask(t.ID)
		if ok && prior.SessionID != "" {
			continue
		}
		c := ChildStart{SessionID: t.SessionID}
		if ok {
			c.Proposal = EntryRef(EntryTask, prior.ID, prior)
		}
		out = append(out, c)
	}
	return out
}

// Delivery is one task result delivered between two states: the child and the mark delivered.
type Delivery struct {
	SessionID string
	Seq       int64
}

// Deliveries lists the task results whose delivered mark differs between before and after, ordered by
// child session.
func Deliveries(before, after State) []Delivery {
	var out []Delivery
	for id, mark := range after.TaskResults {
		if prior, ok := before.TaskResults[id]; ok && prior == mark {
			continue
		}
		out = append(out, Delivery{SessionID: id, Seq: mark.Seq})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}
