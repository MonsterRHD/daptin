package server

import (
	"errors"
	"strings"

	"github.com/artpar/go-imap"
	"github.com/artpar/go-imap/commands"
	"github.com/artpar/go-imap/responses"
)

// qresyncCapableMailbox is implemented by mailboxes that can serve
// CONDSTORE/QRESYNC data. The concrete backend mailbox implements it.
type qresyncCapableMailbox interface {
	HighestModSequence() (int64, error)
	VanishedSince(since int64, knownUIDs *imap.SeqSet) (*imap.SeqSet, bool, error)
	FetchChanged(uid bool, seqset *imap.SeqSet, items []imap.FetchItem,
		since int64, ch chan<- *imap.Message) error
}

// highestModSeqStatus builds the untagged OK response carrying HIGHESTMODSEQ.
func highestModSeqStatus(value int64) *imap.StatusResp {
	return &imap.StatusResp{
		Tag:       "*",
		Type:      imap.StatusRespOk,
		Code:      imap.StatusRespCode("HIGHESTMODSEQ"),
		Arguments: []interface{}{value},
	}
}

// vanishedResp writes the VANISHED response. With earlier=true it is the
// VANISHED (EARLIER) form used during QRESYNC selection; without it, it is
// the form used after EXPUNGE.
type vanishedResp struct {
	earlier bool
	set     *imap.SeqSet
}

func (r *vanishedResp) WriteTo(w *imap.Writer) error {
	fields := []interface{}{imap.RawString("VANISHED")}
	if r.earlier {
		fields = append(fields, imap.RawString("(EARLIER)"))
	}
	fields = append(fields, imap.RawString(r.set.String()))
	return imap.NewUntaggedResp(fields).WriteTo(w)
}

// ---------------------------------------------------------------------------
// SELECT / EXAMINE
// ---------------------------------------------------------------------------

type qresyncParam struct {
	uidValidity uint32
	modseq      int64
	uidSet      *imap.SeqSet
}

type condStoreSelect struct {
	commands.Select
	condstore bool
	qresync   *qresyncParam
}

func (cmd *condStoreSelect) Parse(fields []interface{}) error {
	if err := cmd.Select.Parse(fields); err != nil {
		return err
	}
	for _, f := range fields[1:] {
		list, ok := f.([]interface{})
		if !ok {
			return errors.New("SELECT parameter must be a parenthesized list")
		}
		if len(list) == 0 {
			continue
		}
		name, err := imap.ParseString(list[0])
		if err != nil {
			return err
		}
		switch strings.ToUpper(name) {
		case "CONDSTORE":
			cmd.condstore = true
		case "QRESYNC":
			p, err := parseQResyncParams(list[1:])
			if err != nil {
				return err
			}
			cmd.qresync = p
		}
	}
	return nil
}

func parseQResyncParams(fields []interface{}) (*qresyncParam, error) {
	if len(fields) < 2 {
		return nil, errors.New("QRESYNC requires UIDVALIDITY and MODSEQ")
	}
	uidValidity, err := imap.ParseNumber(fields[0])
	if err != nil {
		return nil, err
	}
	modseq, err := imap.ParseNumber(fields[1])
	if err != nil {
		return nil, err
	}
	p := &qresyncParam{uidValidity: uidValidity, modseq: int64(modseq)}
	if len(fields) > 2 {
		setString, err := imap.ParseString(fields[2])
		if err != nil {
			return nil, err
		}
		if p.uidSet, err = imap.ParseSeqSet(setString); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (cmd *condStoreSelect) Handle(conn Conn) error {
	ctx := conn.Context()
	ctx.Mailbox = nil
	ctx.MailboxReadOnly = false
	if ctx.User == nil {
		return ErrNotAuthenticated
	}

	state := getConnState(conn)

	mbox, err := ctx.User.GetMailbox(cmd.Mailbox)
	if err != nil {
		return err
	}

	statusItems := []imap.StatusItem{
		imap.StatusMessages, imap.StatusRecent, imap.StatusUnseen,
		imap.StatusUidNext, imap.StatusUidValidity,
	}
	status, err := mbox.Status(statusItems)
	if err != nil {
		return err
	}

	qm, qresyncCapable := mbox.(qresyncCapableMailbox)

	var vanishedSet *imap.SeqSet
	incremental := false
	if cmd.qresync != nil {
		if !state.isEnabled(qresyncCapability) {
			return ErrStatusResp(&imap.StatusResp{
				Type: imap.StatusRespBad,
				Info: "QRESYNC must be enabled using ENABLE before it is used",
			})
		}
		if !qresyncCapable {
			return errors.New("mailbox does not support QRESYNC")
		}
		if cmd.qresync.uidValidity != status.UidValidity {
			// UIDVALIDITY mismatch: RFC 7162 3.1.2.2 — server ignores the
			// QRESYNC parameters and opens the mailbox normally.
			cmd.qresync = nil
		} else {
			set, sufficient, err := qm.VanishedSince(cmd.qresync.modseq, cmd.qresync.uidSet)
			if err != nil {
				return err
			}
			if !sufficient {
				return ErrStatusResp(&imap.StatusResp{
					Type: imap.StatusRespNo,
					Info: "QRESYNC unavailable: expunged message history is incomplete for the synchronization point; re-select without QRESYNC",
				})
			}
			vanishedSet = set
			incremental = true
		}
	}

	ctx.Mailbox = mbox
	ctx.MailboxReadOnly = cmd.ReadOnly || status.ReadOnly

	if err := conn.WriteResp(&responses.Select{Mailbox: status}); err != nil {
		return err
	}

	sendHighest := state.condstoreAware || cmd.condstore || incremental
	if sendHighest {
		state.condstoreAware = true
		highest, err := qm.HighestModSequence()
		if err != nil {
			return err
		}
		if err := conn.WriteResp(highestModSeqStatus(highest)); err != nil {
			return err
		}
	}

	if incremental {
		if !vanishedSet.Empty() {
			if err := conn.WriteResp(&vanishedResp{earlier: true, set: vanishedSet}); err != nil {
				return err
			}
		}

		fetchSet := cmd.qresync.uidSet
		if fetchSet == nil {
			fetchSet = &imap.SeqSet{}
			fetchSet.AddRange(1, ^uint32(0))
		}
		if err := streamFetch(conn, func(ch chan<- *imap.Message) error {
			return qm.FetchChanged(true, fetchSet,
				[]imap.FetchItem{imap.FetchUid, imap.FetchFlags},
				cmd.qresync.modseq, ch)
		}); err != nil {
			return err
		}
	}

	code := imap.CodeReadWrite
	if ctx.MailboxReadOnly {
		code = imap.CodeReadOnly
	}
	return ErrStatusResp(&imap.StatusResp{Type: imap.StatusRespOk, Code: code})
}

// streamFetch runs a FETCH response: the writer is started first, the producer
// function fills the channel, then completion is awaited.
func streamFetch(conn Conn, producer func(ch chan<- *imap.Message) error) error {
	ch := make(chan *imap.Message)
	done := make(chan error, 1)
	go func() {
		done <- conn.WriteResp(&responses.Fetch{Messages: ch})
		for range ch {
		}
	}()

	if err := producer(ch); err != nil {
		return err
	}
	return <-done
}

// ---------------------------------------------------------------------------
// FETCH
// ---------------------------------------------------------------------------

type condStoreFetch struct {
	commands.Fetch
	changedSince int64
	withVanished bool
}

func (cmd *condStoreFetch) Parse(fields []interface{}) error {
	if len(fields) < 2 {
		return errors.New("not enough arguments")
	}
	setString, err := imap.ParseString(fields[0])
	if err != nil {
		return err
	}
	if cmd.SeqSet, err = imap.ParseSeqSet(setString); err != nil {
		return err
	}

	switch items := fields[1].(type) {
	case string:
		cmd.Items = imap.FetchItem(strings.ToUpper(items)).Expand()
	case []interface{}:
		cmd.Items = make([]imap.FetchItem, 0, len(items))
		for _, v := range items {
			itemString, _ := v.(string)
			cmd.Items = append(cmd.Items, imap.FetchItem(strings.ToUpper(itemString)).Expand()...)
		}
	default:
		return errors.New("items must be either an atom or a list")
	}

	for _, f := range fields[2:] {
		switch v := f.(type) {
		case string:
			if strings.ToUpper(v) == "VANISHED" {
				cmd.withVanished = true
			}
		case []interface{}:
			if len(v) >= 2 {
				if name, _ := imap.ParseString(v[0]); strings.ToUpper(name) == "CHANGEDSINCE" {
					n, err := imap.ParseNumber(v[1])
					if err != nil {
						return err
					}
					cmd.changedSince = int64(n)
				}
			}
		}
	}
	return nil
}

func (cmd *condStoreFetch) Handle(conn Conn) error {
	return cmd.handle(false, conn)
}

func (cmd *condStoreFetch) UidHandle(conn Conn) error {
	hasUid := false
	for _, item := range cmd.Items {
		if item == imap.FetchUid {
			hasUid = true
			break
		}
	}
	if !hasUid {
		cmd.Items = append(cmd.Items, imap.FetchUid)
	}
	return cmd.handle(true, conn)
}

func (cmd *condStoreFetch) handle(uid bool, conn Conn) error {
	ctx := conn.Context()
	if ctx.Mailbox == nil {
		return ErrNoMailboxSelected
	}

	state := getConnState(conn)
	qm, qresyncCapable := ctx.Mailbox.(qresyncCapableMailbox)

	if cmd.withVanished {
		if !uid {
			return ErrStatusResp(&imap.StatusResp{
				Type: imap.StatusRespBad,
				Info: "VANISHED is only allowed with UID FETCH",
			})
		}
		if cmd.changedSince == 0 {
			return ErrStatusResp(&imap.StatusResp{
				Type: imap.StatusRespBad,
				Info: "VANISHED must be used together with CHANGEDSINCE",
			})
		}
		if !state.isEnabled(qresyncCapability) {
			return ErrStatusResp(&imap.StatusResp{
				Type: imap.StatusRespBad,
				Info: "QRESYNC must be enabled before VANISHED can be requested",
			})
		}
	}
	if cmd.changedSince > 0 {
		state.condstoreAware = true
	}

	var vanishedSet *imap.SeqSet
	if cmd.withVanished {
		set, sufficient, err := qm.VanishedSince(cmd.changedSince, cmd.SeqSet)
		if err != nil {
			return err
		}
		if !sufficient {
			return ErrStatusResp(&imap.StatusResp{
				Type: imap.StatusRespNo,
				Info: "Expunged message history is incomplete for the synchronization point",
			})
		}
		vanishedSet = set
	}

	// VANISHED (EARLIER) is sent before FETCH responses.
	if vanishedSet != nil && !vanishedSet.Empty() {
		if err := conn.WriteResp(&vanishedResp{earlier: true, set: vanishedSet}); err != nil {
			return err
		}
	}

	items := cmd.Items
	if cmd.changedSince > 0 {
		return streamFetch(conn, func(ch chan<- *imap.Message) error {
			return qm.FetchChanged(uid, cmd.SeqSet, items, cmd.changedSince, ch)
		})
	}
	return streamFetch(conn, func(ch chan<- *imap.Message) error {
		return ctx.Mailbox.ListMessages(uid, cmd.SeqSet, items, ch)
	})
}

// ---------------------------------------------------------------------------
// STORE
// ---------------------------------------------------------------------------

type condStoreStore struct {
	commands.Store
	unchangedSince int64
	hasCondition   bool
}

func (cmd *condStoreStore) Parse(fields []interface{}) error {
	if len(fields) < 3 {
		return errors.New("not enough arguments")
	}
	setString, err := imap.ParseString(fields[0])
	if err != nil {
		return err
	}
	if cmd.SeqSet, err = imap.ParseSeqSet(setString); err != nil {
		return err
	}
	itemString, err := imap.ParseString(fields[1])
	if err != nil {
		return errors.New("item must be an atom")
	}
	cmd.Item = imap.StoreItem(strings.ToUpper(itemString))

	remaining := make([]interface{}, 0, len(fields)-2)
	for _, f := range fields[2:] {
		if list, isList := f.([]interface{}); isList && len(list) >= 2 {
			if name, _ := imap.ParseString(list[0]); strings.ToUpper(name) == "UNCHANGEDSINCE" {
				n, err := imap.ParseNumber(list[1])
				if err != nil {
					return err
				}
				cmd.unchangedSince = int64(n)
				cmd.hasCondition = true
				continue
			}
		}
		remaining = append(remaining, f)
	}
	if len(remaining) == 1 {
		cmd.Value = remaining[0]
	} else {
		cmd.Value = remaining
	}
	return nil
}

func (cmd *condStoreStore) Handle(conn Conn) error {
	return cmd.handle(false, conn)
}

func (cmd *condStoreStore) UidHandle(conn Conn) error {
	return cmd.handle(true, conn)
}

func (cmd *condStoreStore) handle(uid bool, conn Conn) error {
	ctx := conn.Context()
	if ctx.Mailbox == nil {
		return ErrNoMailboxSelected
	}
	if ctx.MailboxReadOnly {
		return ErrMailboxReadOnly
	}

	if !cmd.hasCondition {
		// Delegate to the standard STORE flow (flag update + non-silent FETCH).
		std := &Store{}
		std.SeqSet = cmd.SeqSet
		std.Item = cmd.Item
		std.Value = cmd.Value
		return std.handle(uid, conn)
	}

	op, silent, err := imap.ParseFlagsOp(cmd.Item)
	if err != nil {
		return err
	}

	flags, err := parseStoreFlags(cmd.Value)
	if err != nil {
		return err
	}

	*conn.silent() = silent
	qm, qresyncCapable := ctx.Mailbox.(qresyncCapableMailbox)
	if !qresyncCapable {
		*conn.silent() = false
		return errors.New("mailbox does not support UNCHANGEDSINCE")
	}

	modified, succeeded, err := func() (*imap.SeqSet, *imap.SeqSet, error) {
		// The backend method runs the conditional update in its own transaction.
		mbox, ok := ctx.Mailbox.(interface {
			ConditionalStoreFlags(uid bool, seqset *imap.SeqSet, operation imap.FlagsOp,
				flags []string, unchangedSince int64) (*imap.SeqSet, *imap.SeqSet, error)
		})
		if !ok {
			return nil, nil, errors.New("conditional STORE not supported")
		}
		return mbox.ConditionalStoreFlags(uid, cmd.SeqSet, op, flags, cmd.unchangedSince)
	}()
	*conn.silent() = false
	if err != nil {
		return err
	}

	// RFC 7162 3.1.1: unsolicited MODSEQ FETCH for messages changed by this
	// STORE, even when .SILENT is used.
	if !succeeded.Empty() {
		fetchItems := []imap.FetchItem{imap.FetchFlags, "MODSEQ"}
		if uid {
			fetchItems = append([]imap.FetchItem{imap.FetchUid}, fetchItems...)
		}
		if err := streamFetch(conn, func(ch chan<- *imap.Message) error {
			return qm.FetchChanged(uid, succeeded, fetchItems, cmd.unchangedSince, ch)
		}); err != nil {
			return err
		}
	}

	if !modified.Empty() {
		return ErrStatusResp(&imap.StatusResp{
			Type:      imap.StatusRespOk,
			Code:      imap.StatusRespCode("MODIFIED"),
			Arguments: []interface{}{imap.RawString(modified.String())},
		})
	}
	return ErrStatusResp(&imap.StatusResp{Type: imap.StatusRespOk})
}

func parseStoreFlags(value interface{}) ([]string, error) {
	if flagsList, ok := value.([]interface{}); ok {
		strs, err := imap.ParseStringList(flagsList)
		if err != nil {
			return nil, err
		}
		for i, f := range strs {
			strs[i] = imap.CanonicalFlag(f)
		}
		return strs, nil
	}
	str, err := imap.ParseString(value)
	if err != nil {
		return nil, err
	}
	return []string{imap.CanonicalFlag(str)}, nil
}

// ---------------------------------------------------------------------------
// EXPUNGE
// ---------------------------------------------------------------------------

type condStoreExpunge struct {
	commands.Expunge
}

func (cmd *condStoreExpunge) Handle(conn Conn) error {
	ctx := conn.Context()
	if ctx.Mailbox == nil {
		return ErrNoMailboxSelected
	}
	if ctx.MailboxReadOnly {
		return ErrMailboxReadOnly
	}

	state := getConnState(conn)
	if !state.isEnabled(qresyncCapability) {
		// Legacy path: per-message EXPUNGE responses.
		return (&Expunge{}).Handle(conn)
	}

	qm, qresyncCapable := ctx.Mailbox.(qresyncCapableMailbox)
	if !qresyncCapable {
		return errors.New("mailbox does not support QRESYNC")
	}

	// UIDs of messages that will be removed; UIDs are unaffected by expunging.
	criteria := &imap.SearchCriteria{WithFlags: []string{imap.DeletedFlag}}
	uids, err := ctx.Mailbox.SearchMessages(true, criteria)
	if err != nil {
		return err
	}

	if err := ctx.Mailbox.Expunge(); err != nil {
		return err
	}

	set := &imap.SeqSet{}
	set.AddNum(uids...)
	if !set.Empty() {
		if err := conn.WriteResp(&vanishedResp{set: set}); err != nil {
			return err
		}
	}

	highest, err := qm.HighestModSequence()
	if err != nil {
		return err
	}
	return ErrStatusResp(&imap.StatusResp{
		Type:      imap.StatusRespOk,
		Code:      imap.StatusRespCode("HIGHESTMODSEQ"),
		Arguments: []interface{}{highest},
	})
}
