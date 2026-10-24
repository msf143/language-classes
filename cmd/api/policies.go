package main

// GET /policies: every number the public policies page states, from the same
// places the code that enforces them reads: Go constants for the booking
// rules, the credit_policy row for expiry, the teachers' own break length. So
// the page can't promise something the code doesn't do.

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

const maxPartySize = 4 // one booking covers 1 to 4 people (also booking.party_size's CHECK)

type policiesDTO struct {
	FullRefundHours      int `json:"full_refund_hours"`       // cancel more than this before the start: everything back
	LateRefundPercent    int `json:"late_refund_percent"`     // later (until the start): this share back
	GroupMinStudents     int `json:"group_min_students"`      // a group class runs with at least this many
	GroupDecisionHours   int `json:"group_decision_hours"`    // decided (runs or cancelled) this long before
	PrivateNoticeMinutes int `json:"private_notice_minutes"`  // private classes bookable from this far ahead
	MaxPartySize         int `json:"max_party_size"`          // people in one private booking
	BreakMinutes         int `json:"break_minutes"`           // a class may end this much early (teacher's break)
	CreditExpireMonths   int `json:"credit_expire_months"`    // credit expires after this long without activity
	CreditWarnDays       int `json:"credit_warn_days_before"` // reminder email this many days before
}

// getPolicies handles GET /policies (public).
func getPolicies(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := policiesDTO{
			FullRefundHours:      int(fullRefundNotice.Hours()),
			LateRefundPercent:    lateCancelRefundPercent,
			GroupMinStudents:     groupMinStudents,
			GroupDecisionHours:   int(groupDecisionNotice.Hours()),
			PrivateNoticeMinutes: int(privateMinNotice.Minutes()),
			MaxPartySize:         maxPartySize,
		}
		// The longest break any active teacher takes, so the page never
		// understates it.
		err := pool.QueryRow(r.Context(), `
			SELECT COALESCE((SELECT MAX(buffer_minutes) FROM teacher WHERE status = 'active'), 0),
			       expire_after_months, warn_days_before
			FROM credit_policy`).Scan(&p.BreakMinutes, &p.CreditExpireMonths, &p.CreditWarnDays)
		if err != nil {
			logFrom(r).Error("policies: query failed", "err", err)
			writeError(w, r, http.StatusInternalServerError, "could not load the policies")
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}
