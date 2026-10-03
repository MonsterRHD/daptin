package scim

// maxFilterResults matches the declared filter capability and caps list counts.
const maxFilterResults = 1000

// DefaultCount is used when a client omits count.
const DefaultCount = 100

type spcFeature struct {
	Supported bool `json:"supported"`
}

type spcFilter struct {
	Supported  bool `json:"supported"`
	MaxResults int  `json:"maxResults"`
}

type spcBulk struct {
	Supported      bool `json:"supported"`
	MaxOperations  int  `json:"maxOperations"`
	MaxPayloadSize int  `json:"maxPayloadSize"`
}

type authenticationScheme struct {
	Type             string `json:"type"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	SpecURI          string `json:"specUri,omitempty"`
	DocumentationURI string `json:"documentationUri,omitempty"`
}

// ServiceProviderConfigDocument is the RFC 7643 §5 configuration resource.
type ServiceProviderConfigDocument struct {
	Schemas               []string                        `json:"schemas"`
	DocumentationURI      string                          `json:"documentationUri,omitempty"`
	Patch                 spcFeature                      `json:"patch"`
	Bulk                  spcBulk                         `json:"bulk"`
	Filter                spcFilter                       `json:"filter"`
	ChangePassword        spcFeature                      `json:"changePassword"`
	Sort                  spcFeature                      `json:"sort"`
	ETag                  spcFeature                      `json:"etag"`
	AuthenticationSchemes map[string]authenticationScheme `json:"authenticationSchemes"`
}

// ServiceProviderConfig returns the static capability document. It must only
// describe capabilities implemented by this service.
func ServiceProviderConfig() ServiceProviderConfigDocument {
	return ServiceProviderConfigDocument{
		Schemas:          []string{ServiceConfigURI},
		DocumentationURI: "https://datatracker.ietf.org/doc/html/rfc7644",
		Patch:            spcFeature{Supported: true},
		Bulk:             spcBulk{Supported: false},
		Filter:           spcFilter{Supported: true, MaxResults: maxFilterResults},
		ChangePassword:   spcFeature{Supported: true},
		Sort:             spcFeature{Supported: false},
		ETag:             spcFeature{Supported: true},
		AuthenticationSchemes: map[string]authenticationScheme{
			"bearer": {
				Type:        "oauthbearertoken",
				Name:        "Bearer Token",
				Description: "Authentication uses the configured SCIM administrator bearer token.",
				SpecURI:     "https://datatracker.ietf.org/doc/html/rfc6750",
			},
		},
	}
}

// ResourceTypeDocument describes one SCIM resource type.
type ResourceTypeDocument struct {
	Schemas          []string            `json:"schemas"`
	ID               string              `json:"id"`
	Name             string              `json:"name"`
	Endpoint         string              `json:"endpoint"`
	Description      string              `json:"description"`
	Schema           string              `json:"schema"`
	SchemaExtensions []map[string]string `json:"schemaExtensions"`
}

// ResourceTypes returns the two supported resource type documents.
func ResourceTypes() []ResourceTypeDocument {
	return []ResourceTypeDocument{
		{
			Schemas:     []string{ResourceTypeURI},
			ID:          "User",
			Name:        "User",
			Endpoint:    UsersPath,
			Description: "User accounts mapped to Daptin user_account rows.",
			Schema:      UserSchemaURI,
		},
		{
			Schemas:     []string{ResourceTypeURI},
			ID:          "Group",
			Name:        "Group",
			Endpoint:    GroupsPath,
			Description: "User groups mapped to Daptin usergroup rows.",
			Schema:      GroupSchemaURI,
		},
	}
}

// ResourceType returns the named resource type document, false when absent.
func ResourceType(name string) (ResourceTypeDocument, bool) {
	for _, doc := range ResourceTypes() {
		if doc.ID == name {
			return doc, true
		}
	}
	return ResourceTypeDocument{}, false
}

// SchemaAttribute describes one attribute in a SCIM schema document.
type SchemaAttribute struct {
	Name          string            `json:"name"`
	Type          string            `json:"type"`
	MultiValued   bool              `json:"multiValued"`
	Description   string            `json:"description,omitempty"`
	Required      bool              `json:"required,omitempty"`
	CaseExact     bool              `json:"caseExact,omitempty"`
	Mutability    string            `json:"mutability"`
	Returned      string            `json:"returned"`
	Uniqueness    string            `json:"uniqueness,omitempty"`
	SubAttributes []SchemaAttribute `json:"subAttributes,omitempty"`
}

// SchemaDocument is the RFC 7643 schema resource.
type SchemaDocument struct {
	Schemas     []string          `json:"schemas"`
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Attributes  []SchemaAttribute `json:"attributes"`
}

func schemaMetaAttributes() []SchemaAttribute {
	return []SchemaAttribute{
		{
			Name: "resourceType", Type: "string", Mutability: "readOnly",
			Returned: "default",
		},
		{
			Name: "created", Type: "dateTime", Mutability: "readOnly",
			Returned: "default",
		},
		{
			Name: "lastModified", Type: "dateTime", Mutability: "readOnly",
			Returned: "default",
		},
		{
			Name: "location", Type: "reference", Mutability: "readOnly",
			Returned: "default",
		},
		{
			Name: "version", Type: "string", Mutability: "readOnly",
			Returned: "default",
		},
	}
}

// UserSchema returns the supported User schema document.
func UserSchema() SchemaDocument {
	return SchemaDocument{
		Schemas:     []string{SchemaURI},
		ID:          UserSchemaURI,
		Name:        "User",
		Description: "User account",
		Attributes: []SchemaAttribute{
			{Name: "userName", Type: "string", Required: true, Mutability: "readWrite",
				Returned: "default", Uniqueness: "server", CaseExact: false,
				Description: "Unique login identifier, mapped to user_account.email."},
			{
				Name: "name", Type: "complex", Mutability: "readWrite", Returned: "default",
				SubAttributes: []SchemaAttribute{
					{Name: "formatted", Type: "string", Mutability: "readWrite", Returned: "default"},
					{Name: "familyName", Type: "string", Mutability: "writeOnly", Returned: "never"},
					{Name: "givenName", Type: "string", Mutability: "writeOnly", Returned: "never"},
				},
			},
			{Name: "displayName", Type: "string", Mutability: "readWrite", Returned: "default",
				Description: "Mapped to user_account.name."},
			{Name: "active", Type: "boolean", Mutability: "readWrite", Returned: "default"},
			{Name: "password", Type: "string", Mutability: "writeOnly", Returned: "never"},
			{
				Name: "emails", Type: "complex", MultiValued: true, Mutability: "readWrite", Returned: "default",
				SubAttributes: []SchemaAttribute{
					{Name: "value", Type: "string", Mutability: "readWrite", Returned: "default",
						Required: true, CaseExact: true},
					{Name: "display", Type: "string", Mutability: "readOnly", Returned: "default"},
					{Name: "type", Type: "string", Mutability: "readWrite", Returned: "default"},
					{Name: "primary", Type: "boolean", Mutability: "readWrite", Returned: "default"},
				},
			},
			{
				Name: "groups", Type: "complex", MultiValued: true, Mutability: "readOnly", Returned: "default",
				SubAttributes: []SchemaAttribute{
					{Name: "value", Type: "string", Mutability: "readOnly", Returned: "default"},
					{Name: "$ref", Type: "reference", Mutability: "readOnly", Returned: "default"},
					{Name: "display", Type: "string", Mutability: "readOnly", Returned: "default"},
					{Name: "type", Type: "string", Mutability: "readOnly", Returned: "default"},
				},
			},
			{Name: "externalId", Type: "string", Mutability: "readWrite", Returned: "default",
				Uniqueness: "server"},
			{
				Name: "meta", Type: "complex", Mutability: "readOnly", Returned: "default",
				SubAttributes: schemaMetaAttributes(),
			},
		},
	}
}

// GroupSchema returns the supported Group schema document.
func GroupSchema() SchemaDocument {
	return SchemaDocument{
		Schemas:     []string{SchemaURI},
		ID:          GroupSchemaURI,
		Name:        "Group",
		Description: "User group",
		Attributes: []SchemaAttribute{
			{Name: "displayName", Type: "string", Required: true, Mutability: "readWrite",
				Returned: "default", Uniqueness: "server",
				Description: "Mapped to usergroup.name."},
			{Name: "externalId", Type: "string", Mutability: "readWrite", Returned: "default",
				Uniqueness: "server"},
			{
				Name: "members", Type: "complex", MultiValued: true, Mutability: "readWrite", Returned: "default",
				SubAttributes: []SchemaAttribute{
					{Name: "value", Type: "string", Mutability: "readWrite", Returned: "default",
						Required: true},
					{Name: "$ref", Type: "reference", Mutability: "readOnly", Returned: "default"},
					{Name: "display", Type: "string", Mutability: "readOnly", Returned: "default"},
					{Name: "type", Type: "string", Mutability: "readWrite", Returned: "default"},
				},
			},
			{
				Name: "meta", Type: "complex", Mutability: "readOnly", Returned: "default",
				SubAttributes: schemaMetaAttributes(),
			},
		},
	}
}

// MessageSchema returns compact documents for the protocol message schemas.
func MessageSchema(id, name, description string, attributes []SchemaAttribute) SchemaDocument {
	return SchemaDocument{
		Schemas:     []string{SchemaURI},
		ID:          id,
		Name:        name,
		Description: description,
		Attributes:  attributes,
	}
}

// Schemas returns all schemas exposed under /Schemas.
func Schemas() []SchemaDocument {
	return []SchemaDocument{
		UserSchema(),
		GroupSchema(),
		{
			Schemas:     []string{SchemaURI},
			ID:          ServiceConfigURI,
			Name:        "ServiceProviderConfig",
			Description: "Schema for the service provider configuration",
			Attributes: []SchemaAttribute{
				{Name: "patch", Type: "complex", Mutability: "readOnly", Returned: "default"},
				{Name: "bulk", Type: "complex", Mutability: "readOnly", Returned: "default"},
				{Name: "filter", Type: "complex", Mutability: "readOnly", Returned: "default"},
				{Name: "etag", Type: "complex", Mutability: "readOnly", Returned: "default"},
			},
		},
		MessageSchema(ResourceTypeURI, "ResourceType", "Schema for resource type metadata", []SchemaAttribute{
			{Name: "id", Type: "string", Mutability: "readOnly", Returned: "default"},
			{Name: "name", Type: "string", Mutability: "readOnly", Returned: "default"},
			{Name: "endpoint", Type: "string", Mutability: "readOnly", Returned: "default"},
			{Name: "schema", Type: "reference", Mutability: "readOnly", Returned: "default"},
		}),
		MessageSchema(SchemaURI, "Schema", "Schema for describing schemas", []SchemaAttribute{
			{Name: "id", Type: "string", Mutability: "readOnly", Returned: "default"},
			{Name: "name", Type: "string", Mutability: "readOnly", Returned: "default"},
			{Name: "attributes", Type: "complex", MultiValued: true, Mutability: "readOnly", Returned: "default"},
		}),
		MessageSchema(PatchOpURI, "PatchOp", "PATCH request message", []SchemaAttribute{
			{Name: "Operations", Type: "complex", MultiValued: true, Required: true,
				Mutability: "readWrite", Returned: "default"},
		}),
		MessageSchema(ListResponseURI, "ListResponse", "List response message", []SchemaAttribute{
			{Name: "totalResults", Type: "integer", Mutability: "readOnly", Returned: "default"},
			{Name: "startIndex", Type: "integer", Mutability: "readOnly", Returned: "default"},
			{Name: "itemsPerPage", Type: "integer", Mutability: "readOnly", Returned: "default"},
			{Name: "Resources", Type: "complex", MultiValued: true, Mutability: "readOnly", Returned: "default"},
		}),
		MessageSchema(ErrorURI, "Error", "Error response message", []SchemaAttribute{
			{Name: "status", Type: "string", Mutability: "readOnly", Returned: "always"},
			{Name: "scimType", Type: "string", Mutability: "readOnly", Returned: "default"},
			{Name: "detail", Type: "string", Mutability: "readOnly", Returned: "default"},
		}),
	}
}

// SchemaByID returns a schema document by URN, false when unknown.
func SchemaByID(id string) (SchemaDocument, bool) {
	for _, document := range Schemas() {
		if document.ID == id {
			return document, true
		}
	}
	return SchemaDocument{}, false
}
