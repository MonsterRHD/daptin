// Package scim implements the SCIM 2.0 (RFC 7643/7644) provisioning surface
// for Daptin user_account and usergroup resources.
package scim

import "fmt"

const (
	// ContentType is the SCIM protocol media type.
	ContentType = "application/scim+json"

	UserSchemaURI     = "urn:ietf:params:scim:schemas:core:2.0:User"
	GroupSchemaURI    = "urn:ietf:params:scim:schemas:core:2.0:Group"
	ServiceConfigURI  = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	ResourceTypeURI   = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
	SchemaURI         = "urn:ietf:params:scim:schemas:core:2.0:Schema"
	PatchOpURI        = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	ListResponseURI   = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	ErrorURI          = "urn:ietf:params:scim:api:messages:2.0:Error"
	SearchRequestURI  = "urn:ietf:params:scim:api:messages:2.0:SearchRequest"
	BulkRequestURI    = "urn:ietf:params:scim:api:messages:2.0:BulkRequest"
	BulkResponseURI   = "urn:ietf:params:scim:api:messages:2.0:BulkResponse"
	EnterpriseUserURI = "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"

	ResourceTypeUser  = "User"
	ResourceTypeGroup = "Group"

	UsersPath  = "Users"
	GroupsPath = "Groups"
)

// Meta carries RFC 7643 resource metadata.
type Meta struct {
	ResourceType string `json:"resourceType"`
	Created      string `json:"created,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	Location     string `json:"location,omitempty"`
	Version      string `json:"version,omitempty"`
}

// Name is the SCIM user name component. Only formatted is persisted in
// user_account.name; given/family are accepted provisioning inputs.
type Name struct {
	Formatted       string `json:"formatted,omitempty"`
	FamilyName      string `json:"familyName,omitempty"`
	GivenName       string `json:"givenName,omitempty"`
	MiddleName      string `json:"middleName,omitempty"`
	HonorificPrefix string `json:"honorificPrefix,omitempty"`
	HonorificSuffix string `json:"honorificSuffix,omitempty"`
}

// Email is the SCIM e-mail component mapped to user_account.email.
type Email struct {
	Value   string `json:"value,omitempty"`
	Display string `json:"display,omitempty"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

// GroupRef is the read-only group membership listed on a User resource.
type GroupRef struct {
	Type    string `json:"type,omitempty"`
	Value   string `json:"value,omitempty"`
	Ref     string `json:"$ref,omitempty"`
	Display string `json:"display,omitempty"`
}

// User is the SCIM User representation mapped to user_account.
type User struct {
	Schemas     []string   `json:"schemas"`
	ID          string     `json:"id,omitempty"`
	ExternalID  string     `json:"externalId,omitempty"`
	UserName    string     `json:"userName,omitempty"`
	Name        *Name      `json:"name,omitempty"`
	DisplayName string     `json:"displayName,omitempty"`
	NickName    string     `json:"nickName,omitempty"`
	Active      *bool      `json:"active,omitempty"`
	Password    string     `json:"password,omitempty"`
	Emails      []Email    `json:"emails,omitempty"`
	Groups      []GroupRef `json:"groups,omitempty"`
	Meta        *Meta      `json:"meta,omitempty"`
}

// Member references a user that belongs to a SCIM Group.
type Member struct {
	Type    string `json:"type,omitempty"`
	Value   string `json:"value,omitempty"`
	Ref     string `json:"$ref,omitempty"`
	Display string `json:"display,omitempty"`
}

// Group is the SCIM Group representation mapped to usergroup.
type Group struct {
	Schemas     []string `json:"schemas"`
	ID          string   `json:"id,omitempty"`
	ExternalID  string   `json:"externalId,omitempty"`
	DisplayName string   `json:"displayName,omitempty"`
	Members     []Member `json:"members,omitempty"`
	Meta        *Meta    `json:"meta,omitempty"`
}

// ListResponse is the RFC 7643 paged container.
type ListResponse[T any] struct {
	Schemas      []string `json:"schemas"`
	TotalResults int      `json:"totalResults"`
	ItemsPerPage int      `json:"itemsPerPage"`
	StartIndex   int      `json:"startIndex"`
	Resources    []T      `json:"Resources"`
}

// PatchOperation is one RFC 7643 PATCH operation.
type PatchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path,omitempty"`
	Value interface{} `json:"value,omitempty"`
}

// PatchOp is the PATCH request body.
type PatchOp struct {
	Schemas    []string         `json:"schemas"`
	Operations []PatchOperation `json:"Operations"`
}

// ErrorResponse is the SCIM error body.
type ErrorResponse struct {
	Schemas  []string `json:"schemas"`
	Status   string   `json:"status"`
	ScimType string   `json:"scimType,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

// ScimType values defined by RFC 7644 §3.12.
const (
	ScimTypeInvalidFilter = "invalidFilter"
	ScimTypeTooMany       = "tooMany"
	ScimTypeUniqueness    = "uniqueness"
	ScimTypeMutability    = "mutability"
	ScimTypeInvalidSyntax = "invalidSyntax"
	ScimTypeInvalidPath   = "invalidPath"
	ScimTypeNoTarget      = "noTarget"
	ScimTypeInvalidValue  = "invalidValue"
	ScimTypeInvalidVers   = "invalidVers"
	ScimTypeSensitive     = "sensitive"
)

// NewError builds a SCIM error document.
func NewError(status int, scimType, detail string) ErrorResponse {
	statusString := fmt.Sprintf("%d", status)
	return ErrorResponse{
		Schemas:  []string{ErrorURI},
		Status:   statusString,
		ScimType: scimType,
		Detail:   detail,
	}
}
