package web

import (
	"fmt"
	"html/template"
	"time"

	"github.com/oliverbestmann/scantool/internal/store"
)

// actionLabels mirrors the LABELS map in index.html's inline script; keep
// both in sync when adding a new store.Kind.
var actionLabels = map[store.Kind]string{
	store.KindDaemonStarted:    "daemon started",
	store.KindDaemonStopped:    "daemon stopped",
	store.KindKeyPressed:       "key pressed",
	store.KindKeyIgnored:       "key ignored",
	store.KindWebAction:        "requested from web",
	store.KindSessionStarted:   "session started",
	store.KindPageScanned:      "page scanned",
	store.KindScanFailed:       "scan failed",
	store.KindSessionSaved:     "document stored",
	store.KindSessionDiscarded: "session discarded",
	store.KindSaveFailed:       "storing failed",
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

// fmtClock renders t as a German 24h clock, e.g. "14:32:07".
func fmtClock(v any) string {
	t := asTime(v)
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("15:04:05")
}

// fmtDateTime renders t as "10.09.2026, 14:32:07 Uhr".
func fmtDateTime(v any) string {
	t := asTime(v)
	if t.IsZero() {
		return "—"
	}
	return fmtDate(t) + ", " + fmtClock(t) + " Uhr"
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

// actionMeta renders the "session #.. · page .." line under an action item,
// or "" when the action carries no session/page.
func actionMeta(a store.Action) string {
	meta := ""
	if a.SessionID != 0 {
		meta = fmt.Sprintf("session #%d", a.SessionID)
	}
	if a.Page != 0 {
		if meta != "" {
			meta += " · "
		}
		meta += fmt.Sprintf("page %d", a.Page)
	}
	return meta
}

// actionGroup is one day's worth of action log entries, oldest grouping
// unit shown in the UI; actions within stay in their original (newest
// first) order.
type actionGroup struct {
	Key     string
	Day     string
	Actions []store.Action
}

// groupActionsByDay groups actions (assumed newest first) by local
// calendar day, preserving order. Mirrors the grouping done client side in
// renderActions() for the polling path, so the two must produce identical
// markup for morphdom to diff cleanly.
func groupActionsByDay(actions []store.Action) []actionGroup {
	var groups []actionGroup
	for _, a := range actions {
		key := "unknown"
		if !a.Time.IsZero() {
			key = a.Time.Local().Format("2006-01-02")
		}
		if len(groups) == 0 || groups[len(groups)-1].Key != key {
			groups = append(groups, actionGroup{Key: key, Day: fmtDate(a.Time)})
		}
		g := &groups[len(groups)-1]
		g.Actions = append(g.Actions, a)
	}
	return groups
}

var templateFuncs = template.FuncMap{
	"fmtDate":      fmtDate,
	"fmtClock":     fmtClock,
	"fmtDateTime":  fmtDateTime,
	"ago":          ago,
	"actionLabel":  actionLabel,
	"statusBadge":  statusBadge,
	"sessionBadge": sessionBadge,
	"actionMeta":   actionMeta,
	"inc":          inc,
	"groupActions": groupActionsByDay,
}
