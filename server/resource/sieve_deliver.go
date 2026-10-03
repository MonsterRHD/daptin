package resource

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	netmail "net/mail"
	"net/http"
	nettextproto "net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/artpar/api2go/v2"
	"github.com/artpar/parsemail"
	"github.com/daptin/daptin/server/auth"
	sieve "github.com/foxcpp/go-sieve"
	"github.com/foxcpp/go-sieve/interp"
	log "github.com/sirupsen/logrus"
	"github.com/jmoiron/sqlx"
)

// InboundMessageInfo carries SMTP-level context for a local delivery.
type InboundMessageInfo struct {
	Recipient   string
	RemoteIP    string
	TLS         bool
	ContentType string
	ReturnPath  string
	// ListenInterface identifies the SMTP listener that received the message,
	// used to resolve the outbound mail server for redirects.
	ListenInterface string
	// PrimaryHost is used when building a fallback Message-Id, preserving the
	// previous delivery behavior.
	PrimaryHost string
	// Hash is the message identity supplied by the SMTP adapter; when empty
	// the MD5 of the message bytes is used.
	Hash string
}

// ResolveOrCreateMailBox returns the named mailbox of the account, creating
// it safely under the mail-account creation lock when absent.
func (dbResource *DbResource) ResolveOrCreateMailBox(mailAccountId int64, mailBoxName string,
	sessionUser *auth.SessionUser, transaction *sqlx.Tx) (map[string]interface{}, error) {
	box, err := dbResource.GetMailAccountBox(mailAccountId, mailBoxName, transaction)
	if err == nil {
		return box, nil
	}

	if lockErr := dbResource.Cruds["mail_account"].LockMailAccountForMailboxCreation(mailAccountId, transaction); lockErr != nil {
		return nil, fmt.Errorf("could not lock mail account: %w", lockErr)
	}
	box, err = dbResource.GetMailAccountBox(mailAccountId, mailBoxName, transaction)
	if err == nil {
		return box, nil
	}

	accountReference, referenceErr := GetIdToReferenceIdWithTransaction("mail_account", mailAccountId, transaction)
	if referenceErr != nil {
		return nil, referenceErr
	}
	if _, err = dbResource.CreateMailAccountBox(accountReference.String(), sessionUser, mailBoxName, transaction); err != nil {
		return nil, err
	}
	return dbResource.GetMailAccountBox(mailAccountId, mailBoxName, transaction)
}

func trimMailString(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		value = value[:limit]
	}
	return value
}

// inboundMailAttributes builds the mail resource attributes for one copy of
// the message in a mailbox.
func (dbResource *DbResource) inboundMailAttributes(box map[string]interface{}, sessionUser *auth.SessionUser,
	mailBytes []byte, info InboundMessageInfo, spamScore int, uid uint32) (map[string]interface{}, error) {
	hash := strings.TrimSpace(info.Hash)
	if hash == "" {
		hash = GetMD5Hash(mailBytes)
	}

	searchMetadata, metadataErr := ExtractMailSearchMetadata(mailBytes)
	if metadataErr != nil {
		log.Warnf("failed to extract complete mail search metadata; storing raw message: %v", metadataErr)
	}

	parsedMail, parseErr := parsemail.Parse(bytes.NewReader(mailBytes))
	if parseErr != nil {
		log.Warnf("failed to parse mail for metadata: %v", parseErr)
	}

	messageID := trimMailString(searchMetadata.MessageID, 255)
	if messageID == "" {
		userPart := ""
		if at := strings.LastIndex(info.Recipient, "@"); at >= 0 {
			userPart = info.Recipient[:at]
		}
		hostPart := info.PrimaryHost
		if hostPart == "" {
			if at := strings.LastIndex(info.Recipient, "@"); at >= 0 {
				hostPart = info.Recipient[at+1:]
			}
		}
		messageID = fmt.Sprintf("%s.%s@%s", hash, userPart, hostPart)
	}

	var sentDate interface{}
	if !searchMetadata.SentDate.IsZero() {
		sentDate = searchMetadata.SentDate
	}

	flags := "\\Recent"
	if spamScore > 50 {
		flags += ",Spam"
	}

	hasAttachment := false
	if parseErr == nil {
		hasAttachment = len(parsedMail.Attachments) > 0
	}

	return map[string]interface{}{
		"message_id":       messageID,
		"mail_id":          hash,
		"from_address":     searchMetadata.From,
		"to_address":       searchMetadata.To,
		"cc_address":       searchMetadata.Cc,
		"bcc_address":      searchMetadata.Bcc,
		"sender_address":   searchMetadata.Sender,
		"subject":          searchMetadata.Subject,
		"body":             searchMetadata.Body,
		"mail":             dbResource.MailColumnValue("mail", "mail", mailBytes, hash),
		"spam_score":       spamScore,
		"spam":             spamScore > 50,
		"hash":             hash,
		"internal_date":    time.Now(),
		"sent_date":        sentDate,
		"content_type":     trimMailString(info.ContentType, 255),
		"reply_to_address": searchMetadata.ReplyTo,
		"recipient":        trimMailString(info.Recipient, 255),
		"has_attachment":   hasAttachment,
		"ip_addr":          info.RemoteIP,
		"return_path":      trimMailString(info.ReturnPath, 255),
		"is_tls":           info.TLS,
		"mail_box_id":      box["reference_id"],
		"user_account_id":  sessionUser.UserReferenceId,
		"uid":              uid,
		"seen":             false,
		"recent":           true,
		"flags":            flags,
		"size":             len(mailBytes),
	}, nil
}

// appendInboundMail allocates a UID and creates one mail resource in the given
// mailbox inside the supplied transaction.
func (dbResource *DbResource) appendInboundMail(box map[string]interface{}, sessionUser *auth.SessionUser,
	mailBytes []byte, info InboundMessageInfo, spamScore int, transaction *sqlx.Tx) error {
	mailBoxId, ok := box["id"].(int64)
	if !ok || mailBoxId == 0 {
		return errors.New("invalid mailbox id")
	}
	uid, err := dbResource.Cruds["mail_box"].AllocateMailBoxUid(mailBoxId, transaction)
	if err != nil {
		return err
	}
	attributes, err := dbResource.inboundMailAttributes(box, sessionUser, mailBytes, info, spamScore, uid)
	if err != nil {
		return err
	}

	requestURL, _ := url.Parse("/api/mail")
	request := api2go.Request{PlainRequest: (&http.Request{
		Method: http.MethodPost,
		URL:    requestURL,
	}).WithContext(context.WithValue(context.Background(), "user", sessionUser))}

	_, err = dbResource.Cruds["mail"].CreateInboundMailWithTransaction(
		api2go.NewApi2GoModelWithData("mail", nil, 768, nil, attributes), request, transaction)
	return err
}

// DeliverInboundDefault is the existing local-delivery path: spam-scored
// messages over the Spam threshold go to the Spam mailbox, everything else to
// INBOX.
func (dbResource *DbResource) DeliverInboundDefault(mailAccountId int64, sessionUser *auth.SessionUser,
	mailBytes []byte, info InboundMessageInfo, spamScore int, transaction *sqlx.Tx) error {
	mailBoxName := "INBOX"
	if spamScore > 299 {
		mailBoxName = "Spam"
	}
	box, err := dbResource.ResolveOrCreateMailBox(mailAccountId, mailBoxName, sessionUser, transaction)
	if err != nil {
		return err
	}
	return dbResource.appendInboundMail(box, sessionUser, mailBytes, info, spamScore, transaction)
}

// ---------------------------------------------------------------------------
// Sieve execution
// ---------------------------------------------------------------------------

const sieveMaxMailHops = 30

func readMessageHeader(mailBytes []byte) nettextproto.MIMEHeader {
	reader := nettextproto.NewReader(bufio.NewReader(bytes.NewReader(mailBytes)))
	header, err := reader.ReadMIMEHeader()
	if err != nil || header == nil {
		return nettextproto.MIMEHeader{}
	}
	return header
}

// sieveRedirectPolicy enforces loop and duplicate restrictions for redirect.
type sieveRedirectPolicy struct {
	seen         map[string]bool
	envelopeFrom string
	recipient    string
	hops         int
}

func newSieveRedirectPolicy(recipient, envelopeFrom string, header nettextproto.MIMEHeader) *sieveRedirectPolicy {
	return &sieveRedirectPolicy{
		seen:         map[string]bool{},
		envelopeFrom: strings.ToLower(strings.TrimSpace(envelopeFrom)),
		recipient:    strings.ToLower(strings.TrimSpace(recipient)),
		hops:         len(header.Values("Received")),
	}
}

func (p *sieveRedirectPolicy) RedirectAllowed(_ context.Context, _ *interp.RuntimeData, address string) (bool, error) {
	normalized, err := normalizeSieveAddress(address)
	if err != nil {
		return false, err
	}
	// Never redirect back to the original envelope sender or to the recipient's
	// own address; never duplicate an address; stop after too many hops.
	if p.envelopeFrom != "" && normalized == p.envelopeFrom {
		return false, nil
	}
	if normalized == p.recipient {
		return false, nil
	}
	if p.seen[normalized] {
		return false, nil
	}
	if p.hops >= sieveMaxMailHops {
		return false, nil
	}
	p.seen[normalized] = true
	return true, nil
}

func normalizeSieveAddress(address string) (string, error) {
	address = strings.TrimSpace(address)
	parsed, err := netmail.ParseAddress(address)
	if err != nil {
		// Sieve redirect arguments are addr-specs without display names.
		return "", fmt.Errorf("invalid redirect address %q: %w", address, err)
	}
	return strings.ToLower(strings.TrimSpace(parsed.Address)), nil
}

// DeliverInboundWithSieve executes the recipient's active Sieve script before
// any mailbox write and applies every resulting action inside the transaction.
// When no active script exists, it converges on the default delivery path.
func (dbResource *DbResource) DeliverInboundWithSieve(mailAccountId int64, sessionUser *auth.SessionUser,
	mailBytes []byte, info InboundMessageInfo, envelopeFrom string, spamScore int,
	transaction *sqlx.Tx) error {
	_, content, active, err := dbResource.GetActiveSieveScript(mailAccountId, transaction)
	if err != nil {
		return err
	}
	if !active {
		return dbResource.DeliverInboundDefault(mailAccountId, sessionUser, mailBytes, info, spamScore, transaction)
	}

	script, err := sieve.Load(strings.NewReader(content), sieve.DefaultOptions())
	if err != nil {
		return fmt.Errorf("failed to parse active sieve script: %w", err)
	}

	header := readMessageHeader(mailBytes)
	message := interp.MessageStatic{
		Size:       len(mailBytes),
		Header:     header,
		RawMessage: mailBytes,
	}
	envelope := interp.EnvelopeStatic{
		From: envelopeFrom,
		To:   info.Recipient,
	}
	policy := newSieveRedirectPolicy(info.Recipient, envelopeFrom, header)
	runtimeData := sieve.NewRuntimeData(script, policy, envelope, message)

	if err := script.Execute(context.Background(), runtimeData); err != nil {
		return fmt.Errorf("sieve execution failed: %w", err)
	}

	return dbResource.applySieveActions(mailAccountId, sessionUser, mailBytes, info,
		envelopeFrom, spamScore, header, runtimeData, transaction)
}

func (dbResource *DbResource) applySieveActions(mailAccountId int64, sessionUser *auth.SessionUser,
	mailBytes []byte, info InboundMessageInfo, envelopeFrom string, spamScore int, header nettextproto.MIMEHeader,
	runtimeData *interp.RuntimeData, transaction *sqlx.Tx) error {
	boxesWritten := map[string]bool{}

	for _, action := range runtimeData.AppliedActions {
		switch a := action.(type) {
		case interp.ActionKeep:
			if err := dbResource.writeSieveMailboxCopy(mailAccountId, "INBOX", boxesWritten,
				sessionUser, mailBytes, info, spamScore, transaction); err != nil {
				return err
			}
		case interp.ActionFileInto:
			if err := dbResource.writeSieveMailboxCopy(mailAccountId, a.Mailbox, boxesWritten,
				sessionUser, mailBytes, info, spamScore, transaction); err != nil {
				return err
			}
		case interp.ActionRedirect:
			if err := dbResource.EnqueueOutboxMail(envelopeFrom, a.Address,
				info.ListenInterface, mailBytes, transaction); err != nil {
				return err
			}
		case interp.ActionVacation:
			if err := dbResource.handleVacationAction(a, mailAccountId, sessionUser,
				mailBytes, info, header, transaction); err != nil {
				return err
			}
		case interp.ActionReject:
			if err := dbResource.enqueueRejectReply(a.Reason, false, envelopeFrom,
				info, header, transaction); err != nil {
				return err
			}
		case interp.ActionEReject:
			if err := dbResource.enqueueRejectReply(a.Reason, true, envelopeFrom,
				info, header, transaction); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeSieveMailboxCopy stores one copy of the message in the target mailbox,
// deduplicating repeated delivery to the same mailbox.
func (dbResource *DbResource) writeSieveMailboxCopy(mailAccountId int64, mailboxName string,
	boxesWritten map[string]bool, sessionUser *auth.SessionUser, mailBytes []byte, info InboundMessageInfo,
	spamScore int, transaction *sqlx.Tx) error {
	box, err := dbResource.ResolveOrCreateMailBox(mailAccountId, mailboxName, sessionUser, transaction)
	if err != nil {
		return err
	}
	boxId, _ := box["id"].(int64)
	key := fmt.Sprintf("%d", boxId)
	if boxesWritten[key] {
		return nil
	}
	if err := dbResource.appendInboundMail(box, sessionUser, mailBytes, info, spamScore, transaction); err != nil {
		return err
	}
	boxesWritten[key] = true
	return nil
}

// enqueueRejectReply sends the rejection response for reject/ereject. No reply
// is produced for a null envelope sender, and the reply is queued with an empty
// MAIL FROM and an Auto-Submitted header to avoid loops.
func (dbResource *DbResource) enqueueRejectReply(reason string, ereject bool, envelopeFrom string,
	info InboundMessageInfo, header nettextproto.MIMEHeader, transaction *sqlx.Tx) error {
	if strings.TrimSpace(envelopeFrom) == "" {
		return nil
	}
	subject := "Automatically rejected message"
	if ereject {
		subject = "Message rejected"
	}
	if originalSubject := strings.TrimSpace(firstHeaderValue(header, "Subject")); originalSubject != "" {
		subject += ": " + originalSubject
	}
	reply := buildAutoReplyMessage(autoReplySpec{
		to:         envelopeFrom,
		from:       info.Recipient,
		subject:    subject,
		inReplyTo:  firstHeaderValue(header, "Message-Id"),
		references: firstHeaderValue(header, "Message-Id"),
		body:       reason,
	})
	return dbResource.EnqueueOutboxMail("", envelopeFrom, info.ListenInterface, reply, transaction)
}

func firstHeaderValue(header nettextproto.MIMEHeader, key string) string {
	values := header.Values(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// autoReplySpec describes an automatic response message.
type autoReplySpec struct {
	to         string
	from       string
	subject    string
	inReplyTo  string
	references string
	body       string
}

func buildAutoReplyMessage(spec autoReplySpec) []byte {
	var builder bytes.Buffer
	builder.WriteString("Auto-Submitted: auto-replied\r\n")
	builder.WriteString("MIME-Version: 1.0\r\n")
	writeReplyHeader(&builder, "From", spec.from)
	writeReplyHeader(&builder, "To", spec.to)
	writeReplyHeader(&builder, "Subject", spec.subject)
	writeReplyHeader(&builder, "In-Reply-To", spec.inReplyTo)
	writeReplyHeader(&builder, "References", spec.references)
	builder.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	builder.WriteString("Content-Transfer-Encoding: 7bit\r\n")
	builder.WriteString("\r\n")
	builder.WriteString(spec.body)
	builder.WriteString("\r\n")
	return builder.Bytes()
}

func writeReplyHeader(builder *bytes.Buffer, name, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	builder.WriteString(name + ": " + value + "\r\n")
}
