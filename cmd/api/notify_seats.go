package main

// The worker's emails about seats in private classes (step 9): a friend
// joined or left, and the payer changed the number of seats. The events are
// written in the same transaction as the change (bookings_join.go,
// bookings_seats.go); names that won't be in the database afterwards (someone
// who left or was removed) travel in the payload.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seatEvent is the payload of seat_joined, seat_left and seats_changed.
type seatEvent struct {
	SeatID           string `json:"seat_id"`
	AccountID        string `json:"account_id"`         // the friend (joined / left / removed)
	LearnerName      string `json:"learner_name"`       // their person in the seat
	PartySize        int    `json:"party_size"`         // seats_changed: seats now
	DeltaCents       int    `json:"delta_cents"`        // seats_changed: > 0 paid, < 0 back
	RemovedAccountID string `json:"removed_account_id"` // seats_changed: a friend whose seat was removed
	RemovedLearner   string `json:"removed_learner_name"`
}

// seatEventPayload builds the payload JSON for writing an event.
func seatEventPayload(bookingID string, e seatEvent) []byte {
	b, _ := json.Marshal(struct {
		BookingID string `json:"booking_id"`
		seatEvent
	}{bookingID, e})
	return b
}

// queueSeatEvent writes one seat event into the outbox, inside the caller's transaction.
func queueSeatEvent(ctx context.Context, tx pgx.Tx, eventType, bookingID string, e seatEvent) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO outbox_event (id, aggregate_type, aggregate_id, event_type, payload)
		VALUES ($1, 'booking', $2, $3, $4)`, newUUIDv7(), bookingID, eventType, seatEventPayload(bookingID, e))
	return err
}

func loadPerson(ctx context.Context, pool *pgxpool.Pool, accountID string) (person, error) {
	var p person
	var tz string
	err := pool.QueryRow(ctx, `SELECT display_name, email, timezone FROM account WHERE id = $1`, accountID).
		Scan(&p.name, &p.email, &tz)
	p.loc = zone(tz)
	return p, err
}

func notifySeat(ctx context.Context, pool *pgxpool.Pool, send mailer, siteURL string, ev outboxEvent) error {
	var e seatEvent
	if err := json.Unmarshal(ev.Payload, &e); err != nil {
		return skip("bad payload: " + err.Error())
	}
	f, err := loadBookingFacts(ctx, pool, ev.AggregateID)
	if errors.Is(err, pgx.ErrNoRows) {
		return skip("booking not found")
	}
	if err != nil {
		return err
	}
	if f.status != "confirmed" {
		return skip("booking no longer confirmed") // cancelled meanwhile: that email says it all
	}
	en, es := className(f)
	toPayer := func(subjectEN, subjectES, bodyEN, bodyES string) email {
		return email{
			From: classesEmailFrom, To: f.payer.email, IdempotencyKey: ev.ID + ":payer",
			Subject: subjectEN + ": " + en + " · " + subjectES + ": " + es,
			Text: fmt.Sprintf("Hi %s,\n\n%s\n\n  %s\n  %s\n\nYour bookings: %s\n\n—\n\nHola, %s:\n\n%s\n\n  %s\n  %s\n\nTus reservas: %s\n",
				f.payer.name, bodyEN, en, whenEN(f.startsAt, f.endsAt, f.payer.loc), siteURL,
				f.payer.name, bodyES, es, whenES(f.startsAt, f.endsAt, f.payer.loc), siteURL),
		}
	}

	switch ev.EventType {
	case "seat_joined":
		// Still in the seat? (They may have left again before we got here.)
		var still bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM booking_attendee a JOIN learner l ON l.id = a.learner_id
			               WHERE a.id = $1 AND a.status = 'joined' AND l.account_id = $2)`, e.SeatID, e.AccountID).Scan(&still); err != nil {
			return err
		}
		if !still {
			return skip("no longer in the seat")
		}
		friend, err := loadPerson(ctx, pool, e.AccountID)
		if err != nil {
			return err
		}
		if err := sendLogged(ctx, send, ev, "payer", toPayer("A friend joined", "Se unió alguien",
			fmt.Sprintf("%s joined your class (%s).", friend.name, e.LearnerName),
			fmt.Sprintf("%s se unió a tu clase (%s).", friend.name, e.LearnerName))); err != nil {
			return err
		}
		return sendLogged(ctx, send, ev, "guest", email{
			From: classesEmailFrom, To: friend.email, IdempotencyKey: ev.ID + ":guest",
			Subject: "You're in: " + en + " · Ya estás: " + es,
			Text: fmt.Sprintf(`Hi %[1]s, you're in!

  %[2]s
  %[3]s
  For: %[4]s
  Booked by %[5]s (nothing to pay)

The teacher will send you the video link before the class.
%[8]sYour bookings: %[6]s

—

¡Hola, %[1]s! Ya estás en la clase.

  %[7]s
  %[9]s
  Para: %[4]s
  Reservada por %[5]s (no tienes que pagar nada)

El profesor te enviará el enlace de la videollamada antes de la clase.
%[10]sTus reservas: %[6]s
`, friend.name, en, whenEN(f.startsAt, f.endsAt, friend.loc), e.LearnerName, f.payer.name, siteURL,
				es, breakLineEN(f.breakMinutes), whenES(f.startsAt, f.endsAt, friend.loc), breakLineES(f.breakMinutes)),
		})

	case "seat_left":
		friend, err := loadPerson(ctx, pool, e.AccountID)
		if err != nil {
			return err
		}
		return sendLogged(ctx, send, ev, "payer", toPayer("A friend left", "Alguien salió",
			fmt.Sprintf("%s left your class (%s). The seat is open again: send someone a new link, or remove the seat.", friend.name, e.LearnerName),
			fmt.Sprintf("%s salió de tu clase (%s). El cupo quedó libre: envía un enlace nuevo a otra persona, o quita el cupo.", friend.name, e.LearnerName)))

	case "seats_changed":
		var moneyLineEN, moneyLineES string
		switch {
		case e.DeltaCents > 0:
			moneyLineEN, moneyLineES = moneyEN(e.DeltaCents, f.currency)+" paid from your credit.", moneyES(e.DeltaCents, f.currency)+" pagados con tu crédito."
		case e.DeltaCents < 0:
			moneyLineEN, moneyLineES = moneyEN(-e.DeltaCents, f.currency)+" went back to your credit.", "Se devolvieron "+moneyES(-e.DeltaCents, f.currency)+" a tu crédito."
		}
		if err := sendLogged(ctx, send, ev, "payer", toPayer("Seats changed", "Cupos cambiados",
			fmt.Sprintf("Your class now has %d seat(s). %s", e.PartySize, moneyLineEN),
			fmt.Sprintf("Tu clase ahora tiene %d cupo(s). %s", e.PartySize, moneyLineES))); err != nil {
			return err
		}
		if e.RemovedAccountID == "" {
			return nil
		}
		friend, err := loadPerson(ctx, pool, e.RemovedAccountID)
		if err != nil {
			return err
		}
		return sendLogged(ctx, send, ev, "guest", email{
			From: classesEmailFrom, To: friend.email, IdempotencyKey: ev.ID + ":guest",
			Subject: "Removed from a class: " + en + " · Fuera de una clase: " + es,
			Text: fmt.Sprintf("Hi %[1]s,\n\n%[2]s removed %[3]s from this class:\n\n  %[4]s\n  %[5]s\n\nNothing was charged to you.\n\n—\n\nHola, %[1]s:\n\n%[2]s quitó a %[3]s de esta clase:\n\n  %[6]s\n  %[7]s\n\nNo se te cobró nada.\n",
				friend.name, f.payer.name, e.RemovedLearner, en, whenEN(f.startsAt, f.endsAt, friend.loc),
				es, whenES(f.startsAt, f.endsAt, friend.loc)),
		})
	}
	return skip("no handler for " + ev.EventType)
}
