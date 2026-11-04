package runtime

import (
	"context"

	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/threadctx"
)

// LocatorCatalog is the structured locator index of earlier threads (plan T2-S2): entries of ids,
// enums, current path@blob pairs and configured check ids, and their rendered text. It carries no
// free text of any thread.
type LocatorCatalog struct {
	Entries []threadctx.LocatorEntry `json:"entries"`
	Text    string                   `json:"text"`
}

// locatorCheckRows bounds the eval_results rows read per thread to find the checks it ran.
const locatorCheckRows = 64

// LocatorCatalog builds the locator catalog of this workspace's threads for query, ranked as recall
// ranks them (fenced sessions and exclude's subtree are never candidates), against the main checkout.
// It only reads. Whether and when a request carries the catalog is the push policy's decision
// (O-5, T5a-S2); this method decides nothing about it.
func (e *Engine) LocatorCatalog(ctx context.Context, query, exclude string) (LocatorCatalog, error) {
	return e.locatorCatalog(ctx, query, exclude, e.workspace)
}

func (e *Engine) locatorCatalog(ctx context.Context, query, exclude, workspace string) (LocatorCatalog, error) {
	hits, byID, err := e.rankThreads(ctx, query, threadctx.MaxCatalogEntries, exclude)
	if err != nil {
		return LocatorCatalog{}, err
	}
	configured, err := e.configuredCheckIDs()
	if err != nil {
		return LocatorCatalog{}, err
	}
	threads := make([]threadctx.CatalogThread, 0, len(hits))
	var paths []string
	for _, h := range hits {
		t := byID[h.ID]
		ct := threadctx.CatalogThread{ID: t.ID, Kind: t.Kind, Resolution: t.Resolution, Notes: t.Notes, NoteFacts: map[string]threadctx.SourceFacts{}}
		for _, n := range t.Notes {
			if len(n.Cites) == 0 {
				continue
			}
			st, err := e.store.SourceStatus(ctx, t.ID, taskstate.EntryRef(taskstate.EntryNote, n.ID, n))
			if err != nil {
				return LocatorCatalog{}, err
			}
			ct.NoteFacts[n.ID] = threadctx.SourceFacts{Origin: st.Origin, LineageComplete: st.LineageComplete, Withdrawn: st.Withdrawn}
			for _, c := range n.Cites {
				paths = append(paths, c.Path)
			}
		}
		if len(configured) > 0 {
			rows, err := e.store.EvalResults(ctx, t.ID, 0, locatorCheckRows)
			if err != nil {
				return LocatorCatalog{}, err
			}
			for _, r := range rows {
				if configured[r.CheckID] {
					ct.CheckIDs = append(ct.CheckIDs, r.CheckID)
				}
			}
		}
		threads = append(threads, ct)
	}
	current := map[string]string{}
	if len(paths) > 0 {
		repo, err := repository.New(workspace)
		if err != nil {
			return LocatorCatalog{}, err
		}
		if current, err = repo.FileBlobs(ctx, paths); err != nil {
			return LocatorCatalog{}, err
		}
	}
	entries := threadctx.Catalog(threads, current, threadctx.MaxCatalogEntries)
	if entries == nil {
		entries = []threadctx.LocatorEntry{}
	}
	return LocatorCatalog{Entries: entries, Text: threadctx.RenderCatalog(entries)}, nil
}

// configuredCheckIDs is the set of check ids the workspace configuration defines now: a check the
// configuration no longer defines is not a locator.
func (e *Engine) configuredCheckIDs() (map[string]bool, error) {
	specs, _, err := e.effectiveChecks()
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(specs))
	for _, s := range specs {
		ids[s.ID] = true
	}
	return ids, nil
}
