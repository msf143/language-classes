package main

// Emails the worker sends for outbox events: booking confirmations and
// cancellations, to the payer and (for private classes) the teacher. Plain
// text, English then Spanish, with times in each recipient's own time zone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// notifyHandler returns the worker's event handler: it picks the right email
// for each event type, and skips types it doesn't know (a newer release may
// write events this one doesn't understand yet).
func notifyHandler(pool *pgxpool.Pool, send mailer, siteURL string) eventHandler {
	return func(ctx context.Context, ev outboxEvent) error {
		switch ev.EventType {
		case "booking_created", "booking_cancelled", "booking_reminder":
			return notifyBooking(ctx, pool, send, siteURL, ev)
		case "payment_succeeded":
			return notifyPayment(ctx, pool, send, siteURL, ev)
		default:
			return skip("no handler for " + ev.EventType)
		}
	}
}

// person is someone an email goes to, with where they are.
type person struct {
	name, email string
	loc         *time.Location
}

// bookingFacts is everything a booking email says.
type bookingFacts struct {
	status                  string
	totalCents, refundCents int
	currency                string
	kind, targetLanguage    string
	title                   *string
	startsAt, endsAt        time.Time
	payer, teacher          person
	attendees               string // "Ana, Sofía"
	cancelledBy             string // from the event: "student", "system", ...
}

func notifyBooking(ctx context.Context, pool *pgxpool.Pool, send mailer, siteURL string, ev outboxEvent) error {
	f, err := loadBookingFacts(ctx, pool, ev.AggregateID)
	if errors.Is(err, pgx.ErrNoRows) {
		return skip("booking not found")
	}
	if err != nil {
		return err
	}
	var payload struct {
		CancelledBy string `json:"cancelled_by"`
	}
	_ = json.Unmarshal(ev.Payload, &payload)
	f.cancelledBy = payload.CancelledBy

	var toPayer, toTeacher email
	switch ev.EventType {
	case "booking_created":
		if f.status != "confirmed" {
			return skip("booking no longer confirmed") // cancelled before we got here: that email follows
		}
		toPayer, toTeacher = bookedEmails(f, siteURL)
	case "booking_cancelled":
		toPayer, toTeacher = cancelledEmails(f, siteURL)
	case "booking_reminder":
		if f.status != "confirmed" {
			return skip("booking no longer confirmed") // cancelled since the reminder was queued
		}
		toPayer = reminderEmail(f, siteURL)
		f.kind = "" // reminders go to the payer only; the teacher has his dashboard
	}

	toPayer.IdempotencyKey = ev.ID + ":payer"
	if err := sendLogged(ctx, send, ev, "payer", toPayer); err != nil {
		return err
	}
	// The teacher hears about private classes (group classes he set up himself).
	if f.kind == "private" {
		toTeacher.IdempotencyKey = ev.ID + ":teacher"
		if err := sendLogged(ctx, send, ev, "teacher", toTeacher); err != nil {
			return err // a retry resends the payer's email too, but its key makes that a no-op
		}
	}
	return nil
}

// sendLogged sends one email and logs it with the provider's message ID (to
// find it in Resend's dashboard). It logs the recipient's role, not their
// address: no personal data in the logs.
func sendLogged(ctx context.Context, send mailer, ev outboxEvent, role string, m email) error {
	id, err := send(ctx, m)
	if err != nil {
		return err
	}
	slog.Info("email sent", "event_id", ev.ID, "event_type", ev.EventType, "to", role, "provider_id", id)
	return nil
}

func loadBookingFacts(ctx context.Context, pool *pgxpool.Pool, bookingID string) (bookingFacts, error) {
	var f bookingFacts
	var payerZone, teacherZone string
	err := pool.QueryRow(ctx, `
		SELECT b.status, b.total_cents, b.refund_cents, b.currency,
		       s.kind, s.target_language, s.title, s.starts_at, s.ends_at,
		       pa.display_name, pa.email, pa.timezone,
		       ta.display_name, ta.email, ta.timezone,
		       COALESCE((SELECT string_agg(l.display_name, ', ' ORDER BY l.display_name)
		                 FROM booking_attendee a JOIN learner l ON l.id = a.learner_id
		                 WHERE a.booking_id = b.id AND a.status <> 'removed'), '')
		FROM booking b
		JOIN session s  ON s.id = b.session_id
		JOIN account pa ON pa.id = b.payer_account_id
		JOIN teacher t  ON t.id = s.teacher_id
		JOIN account ta ON ta.id = t.account_id
		WHERE b.id = $1`, bookingID).
		Scan(&f.status, &f.totalCents, &f.refundCents, &f.currency,
			&f.kind, &f.targetLanguage, &f.title, &f.startsAt, &f.endsAt,
			&f.payer.name, &f.payer.email, &payerZone,
			&f.teacher.name, &f.teacher.email, &teacherZone,
			&f.attendees)
	f.payer.loc, f.teacher.loc = zone(payerZone), zone(teacherZone)
	return f, err
}

// zone loads a time zone by name, falling back to UTC.
func zone(name string) *time.Location {
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.UTC
}

// ---------- the emails ----------

func bookedEmails(f bookingFacts, siteURL string) (toPayer, toTeacher email) {
	en, es := className(f)
	toPayer = email{
		From: classesEmailFrom, To: f.payer.email,
		Subject: "You're booked: " + en + " · Reserva confirmada: " + es,
		Text: fmt.Sprintf(`Hi %[1]s, you're booked!

  %[2]s
  %[3]s
  For: %[4]s
  Paid: %[5]s

The teacher will send you the video link before the class.
Classes may end up to 5 minutes early, so the teacher can take a short break between classes.
Your bookings: %[6]s

—

¡Hola, %[1]s! Tu reserva está confirmada.

  %[7]s
  %[8]s
  Para: %[4]s
  Pagado: %[9]s

El profesor te enviará el enlace de la videollamada antes de la clase.
Las clases pueden terminar hasta 5 minutos antes, para que el profesor tome un breve descanso entre clases.
Tus reservas: %[6]s
`, f.payer.name, en, whenEN(f.startsAt, f.endsAt, f.payer.loc), f.attendees, moneyEN(f.totalCents, f.currency),
			siteURL, es, whenES(f.startsAt, f.endsAt, f.payer.loc), moneyES(f.totalCents, f.currency)),
	}
	toTeacher = email{
		From: classesEmailFrom, To: f.teacher.email,
		Subject: "New private class · Nueva clase privada: " + shortWhen(f.startsAt, f.teacher.loc),
		Text: fmt.Sprintf(`New private class booked.

  %[1]s
  %[2]s
  For: %[3]s
  Booked by: %[4]s <%[5]s>

—

Nueva clase privada reservada.

  %[6]s
  %[7]s
  Para: %[3]s
  Reservada por: %[4]s <%[5]s>
`, en, whenEN(f.startsAt, f.endsAt, f.teacher.loc), f.attendees, f.payer.name, f.payer.email,
			es, whenES(f.startsAt, f.endsAt, f.teacher.loc)),
	}
	return toPayer, toTeacher
}

func cancelledEmails(f bookingFacts, siteURL string) (toPayer, toTeacher email) {
	en, es := className(f)
	whyEN, whyES := "", ""
	if f.cancelledBy == "system" {
		whyEN = "The class didn't reach the minimum number of students, so it was cancelled with a full refund.\n"
		whyES = "La clase no alcanzó el mínimo de estudiantes, así que se canceló con reembolso completo.\n"
	}
	toPayer = email{
		From: classesEmailFrom, To: f.payer.email,
		Subject: "Booking cancelled: " + en + " · Reserva cancelada: " + es,
		Text: fmt.Sprintf(`Hi %[1]s, your booking is cancelled.

  %[2]s
  %[3]s
  For: %[4]s

%[5]s%[6]s went back to your credit.
Your bookings: %[7]s

—

Hola, %[1]s: tu reserva está cancelada.

  %[8]s
  %[9]s
  Para: %[4]s

%[10]sSe devolvieron %[11]s a tu crédito.
Tus reservas: %[7]s
`, f.payer.name, en, whenEN(f.startsAt, f.endsAt, f.payer.loc), f.attendees,
			whyEN, moneyEN(f.refundCents, f.currency), siteURL,
			es, whenES(f.startsAt, f.endsAt, f.payer.loc), whyES, moneyES(f.refundCents, f.currency)),
	}
	toTeacher = email{
		From: classesEmailFrom, To: f.teacher.email,
		Subject: "Private class cancelled · Clase privada cancelada: " + shortWhen(f.startsAt, f.teacher.loc),
		Text: fmt.Sprintf(`A private class was cancelled. The time is open for booking again.

  %[1]s
  %[2]s
  For: %[3]s (booked by %[4]s)

—

Se canceló una clase privada. El horario vuelve a estar disponible.

  %[5]s
  %[6]s
  Para: %[3]s (reservada por %[4]s)
`, en, whenEN(f.startsAt, f.endsAt, f.teacher.loc), f.attendees, f.payer.name,
			es, whenES(f.startsAt, f.endsAt, f.teacher.loc)),
	}
	return toPayer, toTeacher
}

func reminderEmail(f bookingFacts, siteURL string) email {
	en, es := className(f)
	return email{
		From: classesEmailFrom, To: f.payer.email,
		Subject: "Reminder: " + en + " · Recordatorio: " + es,
		Text: fmt.Sprintf(`Hi %[1]s, a reminder: your class is coming up.

  %[2]s
  %[3]s
  For: %[4]s

The teacher will send you the video link before the class.
Classes may end up to 5 minutes early, so the teacher can take a short break between classes.
Your bookings: %[5]s

—

Hola, %[1]s: te recordamos que tu clase se acerca.

  %[6]s
  %[7]s
  Para: %[4]s

El profesor te enviará el enlace de la videollamada antes de la clase.
Las clases pueden terminar hasta 5 minutos antes, para que el profesor tome un breve descanso entre clases.
Tus reservas: %[5]s
`, f.payer.name, en, whenEN(f.startsAt, f.endsAt, f.payer.loc), f.attendees, siteURL,
			es, whenES(f.startsAt, f.endsAt, f.payer.loc)),
	}
}

// notifyPayment emails the receipt for a credit purchase.
func notifyPayment(ctx context.Context, pool *pgxpool.Pool, send mailer, siteURL string, ev outboxEvent) error {
	var name, to, currency, status string
	var paid, bonus, balance int
	err := pool.QueryRow(ctx, `
		SELECT a.display_name, a.email, p.status, p.amount_cents, p.bonus_cents, p.currency,
		       COALESCE((SELECT SUM(l.amount_cents) FROM credit_ledger l
		                 WHERE l.account_id = a.id AND l.currency = p.currency), 0)
		FROM payment p JOIN account a ON a.id = p.account_id
		WHERE p.id = $1`, ev.AggregateID).Scan(&name, &to, &status, &paid, &bonus, &currency, &balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return skip("payment not found")
	}
	if err != nil {
		return err
	}
	if status != "succeeded" {
		return skip("payment not succeeded")
	}
	added := paid + bonus
	bonusEN, bonusES := "", ""
	if bonus > 0 {
		bonusEN = fmt.Sprintf(" (%s paid + %s bonus)", moneyEN(paid, currency), moneyEN(bonus, currency))
		bonusES = fmt.Sprintf(" (%s pagados + %s de bono)", moneyES(paid, currency), moneyES(bonus, currency))
	}
	return sendLogged(ctx, send, ev, "payer", email{
		From: classesEmailFrom, To: to, IdempotencyKey: ev.ID + ":payer",
		Subject: "Credit added: " + moneyEN(added, currency) + " · Crédito añadido: " + moneyES(added, currency),
		Text: fmt.Sprintf(`Hi %[1]s, thank you!

  %[2]s of credit added to your account%[3]s.
  Your credit now: %[4]s

Book a class: %[5]s

—

Hola, %[1]s: ¡gracias!

  Se añadieron %[6]s de crédito a tu cuenta%[7]s.
  Tu crédito ahora: %[8]s

Reserva una clase: %[5]s
`, name, moneyEN(added, currency), bonusEN, moneyEN(balance, currency), siteURL,
			moneyES(added, currency), bonusES, moneyES(balance, currency)),
	})
}

// className: the group class's title, or "Private class (Spanish)".
func className(f bookingFacts) (en, es string) {
	if f.kind == "group" && f.title != nil {
		return *f.title, *f.title
	}
	if f.targetLanguage == "en" {
		return "Private class (English)", "Clase privada (inglés)"
	}
	return "Private class (Spanish)", "Clase privada (español)"
}

// ---------- dates and money in two languages ----------
//
// Go's time formatting knows only English names, so Spanish uses these tables.

var (
	weekdaysES = [...]string{"domingo", "lunes", "martes", "miércoles", "jueves", "viernes", "sábado"}
	monthsES   = [...]string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio",
		"agosto", "septiembre", "octubre", "noviembre", "diciembre"}
)

// whenEN: "Tuesday, October 27, 7:00 PM – 8:00 PM (America/New_York)".
func whenEN(start, end time.Time, loc *time.Location) string {
	s, e := start.In(loc), end.In(loc)
	return fmt.Sprintf("%s, %s – %s (%s)", s.Format("Monday, January 2"), s.Format("3:04 PM"), e.Format("3:04 PM"), loc)
}

// whenES: "martes 27 de octubre, 19:00 – 20:00 (America/New_York)".
func whenES(start, end time.Time, loc *time.Location) string {
	s, e := start.In(loc), end.In(loc)
	return fmt.Sprintf("%s %d de %s, %s – %s (%s)", weekdaysES[s.Weekday()], s.Day(), monthsES[s.Month()-1],
		s.Format("15:04"), e.Format("15:04"), loc)
}

// shortWhen for subjects: "Tue Oct 27 19:00".
func shortWhen(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("Mon Jan 2 15:04")
}

// moneyEN: "$24.00"; moneyES: "24,00 US$" (like the web page shows them).
func moneyEN(cents int, currency string) string {
	return fmt.Sprintf("%s%d.%02d", currencySign(currency), cents/100, cents%100)
}

func moneyES(cents int, currency string) string {
	return fmt.Sprintf("%d,%02d %s", cents/100, cents%100, strings.ReplaceAll(currency, "USD", "US$"))
}

func currencySign(currency string) string {
	if currency == "USD" {
		return "$"
	}
	return currency + " "
}
