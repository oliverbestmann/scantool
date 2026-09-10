package web

import (
	"fmt"
	"html/template"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/oliverbestmann/scantool/internal/store"
)

// actionLabels gives the base heading for each store.Kind. actionTitle folds
// in extra detail (e.g. which key, which file) for kinds where the label
// alone isn't informative enough.
var actionLabels = map[store.Kind]string{
	store.KindDaemonStarted:    "Daemon started",
	store.KindDaemonStopped:    "Daemon stopped",
	store.KindKeyPressed:       "Key pressed",
	store.KindKeyIgnored:       "Key ignored",
	store.KindIdleTimeout:      "Idle timeout, document finished",
	store.KindWebAction:        "Requested from web",
	store.KindSessionStarted:   "Session started",
	store.KindPageScanned:      "Page scanned",
	store.KindScanFailed:       "Scan failed",
	store.KindSessionSaved:     "Document stored",
	store.KindSessionDiscarded: "Session discarded",
	store.KindSaveFailed:       "Storing failed",
	store.KindUploadSucceeded:  "Document uploaded",
	store.KindUploadFailed:     "Upload failed",
}

var statusBadgeClasses = map[string]string{
	"idle":     "text-bg-secondary",
	"scanning": "text-bg-primary",
	"saving":   "text-bg-warning",
	"error":    "text-bg-danger",
}

var sessionBadgeClasses = map[string]string{
	store.StatusActive:    "text-bg-primary",
	store.StatusSaved:     "text-bg-success",
	store.StatusDiscarded: "text-bg-secondary",
	store.StatusFailed:    "text-bg-danger",
}

func actionLabel(k store.Kind) string {
	if l, ok := actionLabels[k]; ok {
		return l
	}
	return string(k)
}

// actionTitle renders the bold heading for an action log entry. For kinds
// whose Detail is a single self-explanatory value (which key, which file),
// it's folded into the heading instead of repeated on the line below.
func actionTitle(a store.Action) string {
	switch a.Kind {
	case store.KindKeyPressed, store.KindKeyIgnored, store.KindWebAction:
		if a.Detail == "" {
			return actionLabel(a.Kind)
		}
		return actionLabel(a.Kind) + ": " + a.Detail
	case store.KindSessionSaved:
		return fmt.Sprintf("%s: %s", actionLabel(a.Kind), pageCount(a.Page))
	case store.KindSessionDiscarded:
		if a.Page == 0 {
			return actionLabel(a.Kind)
		}
		return fmt.Sprintf("%s: %s", actionLabel(a.Kind), pageCount(a.Page))
	default:
		return actionLabel(a.Kind)
	}
}

// pageCount renders n as "1 page" or "N pages".
func pageCount(n int) string {
	if n == 1 {
		return "1 page"
	}
	return fmt.Sprintf("%d pages", n)
}

// actionDetail renders the secondary detail line for an action log entry,
// or "" when actionTitle already folded the detail into the heading.
func actionDetail(a store.Action) string {
	switch a.Kind {
	case store.KindKeyPressed, store.KindKeyIgnored, store.KindWebAction:
		return ""
	case store.KindSessionSaved:
		return filepath.Base(a.Detail)
	default:
		return a.Detail
	}
}

func statusBadge(status any) string {
	if c, ok := statusBadgeClasses[fmt.Sprint(status)]; ok {
		return c
	}
	return "text-bg-secondary"
}

func sessionBadge(status string) string {
	if c, ok := sessionBadgeClasses[status]; ok {
		return c
	}
	return "text-bg-secondary"
}

// asTime accepts the time.Time and *time.Time values used across
// store.Session/store.Action/session.State, so template calls don't need to
// dereference EndedAt themselves.
func asTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case *time.Time:
		if t == nil {
			return time.Time{}
		}
		return *t
	default:
		return time.Time{}
	}
}

// fmtDate renders t as a German date, e.g. "10.09.2026".
func fmtDate(v any) string {
	t := asTime(v)
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("02.01.2006")
}

// fmtClock renders t as a German 24h clock, e.g. "14:32".
func fmtClock(v any) string {
	t := asTime(v)
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("15:04")
}

// fmtDateTime renders t as "10.09.2026, 14:32".
func fmtDateTime(v any) string {
	t := asTime(v)
	if t.IsZero() {
		return "—"
	}
	return fmtDate(t) + ", " + fmtClock(t)
}

// ago renders how long ago t was, relative to now.
func ago(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	secs := int(now.Sub(t).Seconds())
	if secs < 0 {
		secs = 0
	}
	switch {
	case secs < 60:
		return fmt.Sprintf("%ds ago", secs)
	case secs < 3600:
		return fmt.Sprintf("%dm ago", secs/60)
	default:
		return fmt.Sprintf("%dh ago", secs/3600)
	}
}

// inc adds one, since Go templates have no arithmetic operators.
func inc(n int) int { return n + 1 }

// lemmaryLink builds the URL of a session's document on the lemmary server,
// or "" when either baseURL or id is unset.
func lemmaryLink(baseURL, id string) string {
	if baseURL == "" || id == "" {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + "/document/" + id
}

// actionMeta renders the "page .." line under an action item, or "" when
// the action carries no page or belongs to a group header that already
// names the session. For KindSessionSaved, Page is the document's total
// page count, already shown by actionTitle, so it's left out here to avoid
// repeating it.
func actionMeta(a store.Action) string {
	if a.Page != 0 && a.Kind != store.KindSessionSaved && a.Kind != store.KindSessionDiscarded {
		return fmt.Sprintf("page %d", a.Page)
	}
	return ""
}

// actionGroup is one contiguous run of action log entries sharing a session
// (or none). It's an intermediate step towards block, below, used to pick
// out the runs of session-less actions (daemon start/stop) that fall
// between sessions.
type actionGroup struct {
	// ID is the first action's id in the group, used as a stable, unique DOM
	// id: the session id alone would repeat across separate system runs.
	ID        int64
	SessionID int64
	Actions   []store.Action
}

// groupActionsBySession groups actions (assumed sorted by time) into
// contiguous runs sharing a session id, including the pseudo id 0 for
// actions with no session (daemon started/stopped).
func groupActionsBySession(actions []store.Action) []actionGroup {
	var groups []actionGroup
	var lastSessionID int64
	for i, a := range actions {
		if i == 0 || a.SessionID != lastSessionID {
			groups = append(groups, actionGroup{ID: a.ID, SessionID: a.SessionID})
			lastSessionID = a.SessionID
		}
		g := &groups[len(groups)-1]
		g.Actions = append(g.Actions, a)
	}
	return groups
}

// block is one card shown on the page: either a session's full history — its
// actions plus its outcome (status, download, upload) — or a run of actions
// with no session, e.g. a daemon start/stop. Blocks are the unit the
// Documents table and action log used to show separately; combining them
// means a session's actions sit right next to what they produced.
type block struct {
	// ID is a stable, unique DOM id.
	ID      string
	System  bool
	Session store.Session
	// Actions is newest first, matching the rest of the page.
	Actions []store.Action
	// Latest is the block's most recent action's time, or, for a session
	// with none loaded, its start time; used to sort blocks.
	Latest time.Time
}

// buildBlocks merges sessions (with their own full action history) and the
// system-only action runs found in recentActions (daemon started/stopped)
// into one newest-first list of blocks, so the page shows one interleaved
// timeline instead of a documents table and a separate action log.
func buildBlocks(sessions []store.Session, sessionActions map[int64][]store.Action, recentActions []store.Action) []block {
	blocks := make([]block, 0, len(sessions))

	for _, sess := range sessions {
		// SessionActions returns oldest first; the page reads newest first,
		// consistent with everything else on it.
		actions := sessionActions[sess.ID]
		reversed := make([]store.Action, len(actions))
		for i, a := range actions {
			reversed[len(actions)-1-i] = a
		}

		latest := sess.StartedAt
		switch {
		case len(reversed) > 0:
			latest = reversed[0].Time
		case sess.EndedAt != nil:
			latest = *sess.EndedAt
		}

		blocks = append(blocks, block{
			ID:      fmt.Sprintf("session-%d", sess.ID),
			Session: sess,
			Actions: reversed,
			Latest:  latest,
		})
	}

	for _, g := range groupActionsBySession(recentActions) {
		if g.SessionID != 0 {
			// Covered by its own session block above, built from the
			// session's complete history rather than this trimmed window.
			continue
		}
		blocks = append(blocks, block{
			ID:      fmt.Sprintf("system-%d", g.ID),
			System:  true,
			Actions: g.Actions,
			Latest:  g.Actions[0].Time,
		})
	}

	sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].Latest.After(blocks[j].Latest) })
	return blocks
}

var templateFuncs = template.FuncMap{
	"fmtDate":      fmtDate,
	"fmtClock":     fmtClock,
	"fmtDateTime":  fmtDateTime,
	"ago":          ago,
	"actionLabel":  actionLabel,
	"actionTitle":  actionTitle,
	"actionDetail": actionDetail,
	"statusBadge":  statusBadge,
	"sessionBadge": sessionBadge,
	"actionMeta":   actionMeta,
	"pageCount":    pageCount,
	"inc":          inc,
	"base":         filepath.Base,
	"lemmaryLink":  lemmaryLink,
	"sumKB":        sumKB,
	"pagesDesc":    pagesDesc,
}

// pagesDesc returns the 1-based page numbers up to n in descending order, so
// the thumbnail strip can show the newest page first.
func pagesDesc(n int) []int {
	pages := make([]int, n)
	for i := range pages {
		pages[i] = n - i
	}
	return pages
}

// sumKB adds up a session's per-page sizes for the status card's "Size"
// field.
func sumKB(sizes []int64) int64 {
	var sum int64
	for _, kb := range sizes {
		sum += kb
	}
	return sum
}
