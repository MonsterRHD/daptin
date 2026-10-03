package resource

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	nettextproto "net/textproto"
	"strings"
	"time"

	"github.com/daptin/daptin/server/auth"
	"github.com/foxcpp/go-sieve/interp"
	"github.com/jmoiron/sqlx"
)

// vacationListHeaders are the List-* headers that identify mailing list mail.
var vacationListHeaders = []string{
	"List-Id", "List-Help", "List-Unsubscribe", "List-Subscribe",
	"List-Post", "List-Owner", "List-Archive",
}

// vacationEligible applies the RFC 5230 recipient restrictions: no replies to
// mailing lists, automated messages or a null return path.
func vacationEligible(envelopeFrom string, header nettextproto.MIMEHeader) bool {
	if strings.TrimSpace(envelopeFrom) == "" {
		return false
	}
	for _, name := range vacationListHeaders {
		if len(header.Values(name)) > 0 {
			return false
		}
	}
	switch strings.ToLower(strings.TrimSpace(firstHeaderValue(header, "Precedence"))) {
	case "bulk", "list", "junk":
		return false
	}
	if value := strings.ToLower(strings.TrimSpace(firstHeaderValue(header, "Auto-Submitted"))); value != "" && value != "no" {
		return false
	}
	if len(header.Values("X-Autoreply")) > 0 || len(header.Values("X-Autorespond")) > 0 {
		return false
	}
	return true
}

// handleVacationAction performs safety checks, persistent per-sender/rule
// deduplication and queues the vacation reply, all within the transaction.
func (dbResource *DbResource) handleVacationAction(action interp.ActionVacation, mailAccountId int64,
	sessionUser *auth.SessionUser, mailBytes []byte, info InboundMessageInfo,
	header nettextproto.MIMEHeader, transaction *sqlx.Tx) error {

	envelopeFrom := info.ReturnPath
	if !vacationEligible(envelopeFrom, header) {
		return nil
	}
	sender, err := normalizeSieveAddress(envelopeFrom)
	if err != nil {
		return err
	}

	handle := vacationHandle(action)
	trackKey := vacationTrackKey(mailAccountId, sender, handle)

	now := time.Now()
	allowed, err := dbResource.reserveVacationTrack(trackKey, sender, handle, mailAccountId, action.Days, now, transaction)
	if err != nil || !allowed {
		return err
	}

	// With :mime the reason is a complete MIME entity supplied by the script;
	// otherwise the reply is assembled as a text/plain message.
	var reply []byte
	if action.MIME {
		reply = []byte(action.Reason)
	} else {
		reply = buildVacationReply(action, info, header)
	}
	return dbResource.EnqueueOutboxMail("", sender, info.ListenInterface, reply, transaction)
}

// reserveVacationTrack locks the tracking row (FOR UPDATE outside SQLite),
// applies the days window and records the response time, all inside the
// delivery transaction. The track_key unique index guarantees that concurrent
// deliveries to the same sender/rule produce at most one reply.
func (dbResource *DbResource) reserveVacationTrack(trackKey, sender, handle string, mailAccountId int64, days int,
	now time.Time, transaction *sqlx.Tx) (bool, error) {
	query := "select last_sent_at from vacation_track where track_key = ?"
	if !isSQLiteDriver(transaction.DriverName()) {
		query += " for update"
	}

	var raw interface{}
	err := transaction.QueryRowx(query, trackKey).Scan(&raw)
	if err == nil {
		lastSent, parseErr := parseFlexibleTime(raw)
		if parseErr != nil {
			return false, parseErr
		}
		if days != 0 && now.Sub(lastSent) < time.Duration(days)*24*time.Hour {
			return false, nil
		}
		if _, err = transaction.Exec(
			"update vacation_track set last_sent_at = ? where track_key = ?", now, trackKey); err != nil {
			return false, err
		}
		return true, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}

	if _, err = transaction.Exec(
		`insert into vacation_track (track_key, sender, handle, mail_account_id, last_sent_at) values (?, ?, ?, ?, ?)`,
		trackKey, sender, handle, mailAccountId, now); err != nil {
		return false, err
	}
	return true, nil
}

func buildVacationReply(action interp.ActionVacation, info InboundMessageInfo,
	header nettextproto.MIMEHeader) []byte {
	subject := strings.TrimSpace(action.Subject)
	if subject == "" {
		original := strings.TrimSpace(firstHeaderValue(header, "Subject"))
		switch {
		case original == "":
			subject = "Re: your mail"
		case strings.HasPrefix(strings.ToLower(original), "re:"):
			subject = original
		default:
			subject = "Re: " + original
		}
	}

	from := strings.TrimSpace(action.From)
	if from == "" {
		from = info.Recipient
	}

	messageID := firstHeaderValue(header, "Message-Id")
	references := strings.TrimSpace(firstHeaderValue(header, "References"))
	if messageID != "" {
		if references != "" {
			references += " " + messageID
		} else {
			references = messageID
		}
	}

	return buildAutoReplyMessage(autoReplySpec{
		to:         info.ReturnPath,
		from:       from,
		subject:    subject,
		inReplyTo:  messageID,
		references: references,
		body:       action.Reason,
	})
}

// vacationHandle returns the explicit deduplication handle, or a stable handle
// derived from the vacation parameters when none is given.
func vacationHandle(action interp.ActionVacation) string {
	if handle := strings.TrimSpace(action.Handle); handle != "" {
		return handle
	}
	identity := fmt.Sprintf("%d|%s|%s|%s|%t|%s",
		action.Days, action.Subject, action.From, strings.Join(action.Addresses, ","), action.MIME, action.Reason)
	sum := sha256.Sum256([]byte(identity))
	return "auto-" + hex.EncodeToString(sum[:16])
}

// vacationTrackKey builds the portable unique key for a tracking record.
func vacationTrackKey(mailAccountId int64, sender, handle string) string {
	identity := fmt.Sprintf("%d|%s|%s", mailAccountId, sender, handle)
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func parseFlexibleTime(value interface{}) (time.Time, error) {
	switch typed := value.(type) {
	case time.Time:
		return typed, nil
	case string:
		layouts := []string{
			time.RFC3339Nano, time.RFC3339,
			"2006-01-02 15:04:05-07:00", "2006-01-02 15:04:05", "2006-01-02",
		}
		for _, layout := range layouts {
			if parsed, err := time.Parse(layout, typed); err == nil {
				return parsed, nil
			}
		}
		return time.Time{}, fmt.Errorf("cannot parse vacation time %q", typed)
	case nil:
		return time.Time{}, nil
	default:
		return time.Time{}, fmt.Errorf("unsupported vacation time type %T", value)
	}
}
