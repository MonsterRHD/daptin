package resource

import (
	"strings"

	"github.com/daptin/daptin/server/auth"
	daptinid "github.com/daptin/daptin/server/id"
	"github.com/doug-martin/goqu/v9"
	"github.com/jmoiron/sqlx"
)

// Columns carrying the SCIM account lifecycle state.
var (
	ACTIVE_COLUMN_NAME      = auth.ActiveColumn
	EXTERNAL_ID_COLUMN_NAME = "external_id"
)

// RowValueActive normalizes values stored in user_account.active across
// SQL drivers (bool, int64, string) into a boolean. A missing value means
// active for legacy rows predating the column.
func RowValueActive(value interface{}) bool {
	switch v := value.(type) {
	case nil:
		return true
	case bool:
		return v
	case int:
		return v != 0
	case int8:
		return v != 0
	case int16:
		return v != 0
	case int32:
		return v != 0
	case int64:
		return v != 0
	case uint:
		return v != 0
	case uint8:
		return v != 0
	case uint16:
		return v != 0
	case uint32:
		return v != 0
	case uint64:
		return v != 0
	case float32:
		return v != 0
	case float64:
		return v != 0
	case string:
		return parseStoredBool(v)
	case []byte:
		return parseStoredBool(string(v))
	default:
		return true
	}
}

func parseStoredBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "0", "false", "f", "no":
		return false
	default:
		return true
	}
}

// IsUserAccountActiveWithTransaction reports whether the linked user_account
// is allowed to log in. A missing user means inactive for protocol logins.
func IsUserAccountActiveWithTransaction(userReference daptinid.DaptinReferenceId, transaction *sqlx.Tx) (bool, error) {
	rows, err := GetObjectByWhereClauseWithTransaction(USER_ACCOUNT_TABLE_NAME, transaction,
		goqu.Ex{"reference_id": userReference[:]})
	if err != nil {
		return false, err
	}
	if len(rows) < 1 {
		return false, nil
	}
	return RowValueActive(rows[0][ACTIVE_COLUMN_NAME]), nil
}

//
//import (
//	"fmt"
//	"time"
//	"gopkg.in/authboss.v1"
//)
//
//type AuthBossUser struct {
//	ID   int
//	Name string
//
//	// Auth
//	Email    string
//	Password string
//
//	// OAuth2
//	Oauth2Uid      string
//	Oauth2Provider string
//	Oauth2Token    string
//	Oauth2Refresh  string
//	Oauth2Expiry   time.Time
//
//	// Confirm
//	ConfirmToken string
//	Confirmed    bool
//
//	// Lock
//	AttemptNumber int64
//	AttemptTime   time.Time
//	Locked        time.Time
//
//	// Recover
//	RecoverToken       string
//	RecoverTokenExpiry time.Time
//
//	// Remember is in another table
//}
//
//type MemStorer struct {
//	Users    map[string]AuthBossUser
//	Tokens   map[string][]string
//	userCrud *DbResource
//}
//
//func NewMemStorer(userCrud *DbResource) *MemStorer {
//	return &MemStorer{
//		userCrud: userCrud,
//	}
//}
//
//func (s MemStorer) Create(key string, attr authboss.Attributes) error {
//	var user AuthBossUser
//	if err := attr.Bind(&user, true); err != nil {
//		return err
//	}
//	s.Users[key] = user
//	return nil
//}
//
//func (s MemStorer) Put(key string, attr authboss.Attributes) error {
//	return s.Create(key, attr)
//}
//
//func (s MemStorer) Get(key string) (result interface{}, err error) {
//	user, ok := s.Users[key]
//	if !ok {
//		return nil, authboss.ErrUserNotFound
//	}
//
//	return &user, nil
//}
//
//func (s MemStorer) PutOAuth(uid, provider string, attr authboss.Attributes) error {
//	return s.Create(uid+provider, attr)
//}
//
//func (s MemStorer) GetOAuth(uid, provider string) (result interface{}, err error) {
//	user, ok := s.Users[uid+provider]
//	if !ok {
//		return nil, authboss.ErrUserNotFound
//	}
//
//	return &user, nil
//}
//
//func (s MemStorer) AddToken(key, token string) error {
//	s.Tokens[key] = append(s.Tokens[key], token)
//	fmt.Println("AddToken")
//	return nil
//}
//
//func (s MemStorer) DelTokens(key string) error {
//	delete(s.Tokens, key)
//	fmt.Println("DelTokens")
//	return nil
//}
//
//func (s MemStorer) UseToken(givenKey, token string) error {
//	toks, ok := s.Tokens[givenKey]
//	if !ok {
//		return authboss.ErrTokenNotFound
//	}
//
//	for i, tok := range toks {
//		if tok == token {
//			toks[i], toks[len(toks)-1] = toks[len(toks)-1], toks[i]
//			s.Tokens[givenKey] = toks[:len(toks)-1]
//			return nil
//		}
//	}
//
//	return authboss.ErrTokenNotFound
//}
//
//func (s MemStorer) ConfirmUser(tok string) (result interface{}, err error) {
//	fmt.Println("==============", tok)
//
//	for _, u := range s.Users {
//		if u.ConfirmToken == tok {
//			return &u, nil
//		}
//	}
//
//	return nil, authboss.ErrUserNotFound
//}
//
//func (s MemStorer) RecoverUser(rec string) (result interface{}, err error) {
//	for _, u := range s.Users {
//		if u.RecoverToken == rec {
//			return &u, nil
//		}
//	}
//
//	return nil, authboss.ErrUserNotFound
//}
