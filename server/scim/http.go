package scim

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/doug-martin/goqu/v9"
	"github.com/gin-gonic/gin"
)

type httpHandler struct {
	store *Store
	token string
}

// RegisterRoutes mounts the SCIM 2.0 service behind /scim/v2.
func RegisterRoutes(router gin.IRouter, store *Store, adminToken string) {
	handler := &httpHandler{store: store, token: adminToken}
	router.Use(handler.requireBearerToken)

	router.GET("/ServiceProviderConfig", handler.getServiceProviderConfig)
	router.GET("/ResourceTypes", handler.listResourceTypes)
	router.GET("/ResourceTypes/:name", handler.getResourceType)
	router.GET("/Schemas", handler.listSchemas)
	router.GET("/Schemas/:id", handler.getSchema)

	router.GET("/Users", handler.listUsers)
	router.POST("/Users", handler.createUser)
	router.GET("/Users/:id", handler.getUser)
	router.PUT("/Users/:id", handler.replaceUser)
	router.PATCH("/Users/:id", handler.patchUser)
	router.DELETE("/Users/:id", handler.deleteUser)

	router.GET("/Groups", handler.listGroups)
	router.POST("/Groups", handler.createGroup)
	router.GET("/Groups/:id", handler.getGroup)
	router.PUT("/Groups/:id", handler.replaceGroup)
	router.PATCH("/Groups/:id", handler.patchGroup)
	router.DELETE("/Groups/:id", handler.deleteGroup)
}

func (h *httpHandler) requireBearerToken(c *gin.Context) {
	if method := c.Request.Method; method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
		contentType := strings.ToLower(c.ContentType())
		if contentType != "" && contentType != ContentType && !strings.HasPrefix(contentType, "application/json") {
			abortSCIM(c, http.StatusUnsupportedMediaType, ScimTypeInvalidSyntax,
				"content type must be application/scim+json or application/json")
			return
		}
	}
	provided := ""
	if header := c.GetHeader("Authorization"); strings.HasPrefix(strings.ToLower(header), "bearer ") {
		provided = strings.TrimSpace(header[7:])
	} else if key := c.GetHeader("X-API-Key"); key != "" {
		provided = strings.TrimSpace(key)
	}
	if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(h.token)) != 1 {
		c.Header("WWW-Authenticate", `Bearer realm="scim", charset="UTF-8"`)
		abortSCIM(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	c.Next()
}

func abortSCIM(c *gin.Context, status int, scimType, detail string) {
	c.Header("Content-Type", ContentType)
	c.AbortWithStatusJSON(status, NewError(status, scimType, detail))
}

func writeError(c *gin.Context, err error) {
	if apiErr, ok := err.(*APIError); ok {
		abortSCIM(c, apiErr.Status, apiErr.ScimType, apiErr.Detail)
		return
	}
	if filterErr, ok := err.(*FilterError); ok {
		abortSCIM(c, http.StatusBadRequest, ScimTypeInvalidFilter, filterErr.Error())
		return
	}
	abortSCIM(c, http.StatusInternalServerError, "", "internal error")
}

func writeSCIM(c *gin.Context, status int, payload interface{}) {
	c.Header("Content-Type", ContentType)
	c.JSON(status, payload)
}

func baseURL(c *gin.Context) string {
	scheme := c.GetHeader("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
	}
	host := c.GetHeader("X-Forwarded-Host")
	if host == "" {
		host = c.Request.Host
	}
	return scheme + "://" + host + BasePath
}

func ifMatchVersion(c *gin.Context) (*int64, error) {
	header := strings.TrimSpace(c.GetHeader("If-Match"))
	if header == "" {
		return nil, nil
	}
	if header == "*" {
		return nil, nil
	}
	if comma := strings.Index(header, ","); comma >= 0 {
		header = strings.TrimSpace(header[:comma])
	}
	version, err := ParseETag(header)
	if err != nil {
		return nil, preconditionFailed("If-Match must contain a single weak entity tag")
	}
	return &version, nil
}

func decodeJSON(c *gin.Context, target interface{}) bool {
	defer c.Request.Body.Close()
	decoder := json.NewDecoder(c.Request.Body)
	if err := decoder.Decode(target); err != nil {
		writeError(c, &APIError{Status: http.StatusBadRequest, ScimType: ScimTypeInvalidSyntax, Detail: "request body is not valid SCIM JSON"})
		return false
	}
	return true
}

// ---- discovery ----

func (h *httpHandler) getServiceProviderConfig(c *gin.Context) {
	writeSCIM(c, http.StatusOK, ServiceProviderConfig())
}

func (h *httpHandler) listResourceTypes(c *gin.Context) {
	docs := ResourceTypes()
	writeSCIM(c, http.StatusOK, ListResponse[ResourceTypeDocument]{
		Schemas:      []string{ListResponseURI},
		TotalResults: len(docs),
		ItemsPerPage: len(docs),
		StartIndex:   1,
		Resources:    docs,
	})
}

func (h *httpHandler) getResourceType(c *gin.Context) {
	doc, ok := ResourceType(c.Param("name"))
	if !ok {
		abortSCIM(c, http.StatusNotFound, "", "resource type not found")
		return
	}
	writeSCIM(c, http.StatusOK, doc)
}

func (h *httpHandler) listSchemas(c *gin.Context) {
	docs := Schemas()
	writeSCIM(c, http.StatusOK, ListResponse[SchemaDocument]{
		Schemas:      []string{ListResponseURI},
		TotalResults: len(docs),
		ItemsPerPage: len(docs),
		StartIndex:   1,
		Resources:    docs,
	})
}

func (h *httpHandler) getSchema(c *gin.Context) {
	doc, ok := SchemaByID(c.Param("id"))
	if !ok {
		abortSCIM(c, http.StatusNotFound, "", "schema not found")
		return
	}
	writeSCIM(c, http.StatusOK, doc)
}

// ---- users ----

func (h *httpHandler) createUser(c *gin.Context) {
	var input User
	if !decodeJSON(c, &input) {
		return
	}
	user, err := h.store.CreateUser(baseURL(c), &input)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Location", user.Meta.Location)
	c.Header("ETag", user.Meta.Version)
	writeSCIM(c, http.StatusCreated, user)
}

func (h *httpHandler) getUser(c *gin.Context) {
	user, err := h.store.GetUser(baseURL(c), c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("ETag", user.Meta.Version)
	writeSCIM(c, http.StatusOK, user)
}

func (h *httpHandler) listUsers(c *gin.Context) {
	predicate, startIndex, count, ok := listArguments(c, ResourceTypeUser)
	if !ok {
		return
	}
	users, total, err := h.store.ListUsers(baseURL(c), predicate, startIndex, count)
	if err != nil {
		writeError(c, err)
		return
	}
	writeSCIM(c, http.StatusOK, ListResponse[*User]{
		Schemas:      []string{ListResponseURI},
		TotalResults: total,
		ItemsPerPage: len(users),
		StartIndex:   startIndex,
		Resources:    users,
	})
}

func (h *httpHandler) replaceUser(c *gin.Context) {
	version, err := ifMatchVersion(c)
	if err != nil {
		writeError(c, err)
		return
	}
	var input User
	if !decodeJSON(c, &input) {
		return
	}
	user, err := h.store.ReplaceUser(baseURL(c), c.Param("id"), &input, version)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("ETag", user.Meta.Version)
	writeSCIM(c, http.StatusOK, user)
}

func (h *httpHandler) patchUser(c *gin.Context) {
	version, err := ifMatchVersion(c)
	if err != nil {
		writeError(c, err)
		return
	}
	var patch PatchOp
	if !decodeJSON(c, &patch) {
		return
	}
	if len(patch.Operations) == 0 {
		abortSCIM(c, http.StatusBadRequest, ScimTypeInvalidSyntax, "Operations must contain at least one entry")
		return
	}
	user, err := h.store.PatchUser(baseURL(c), c.Param("id"), &patch, version)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("ETag", user.Meta.Version)
	writeSCIM(c, http.StatusOK, user)
}

func (h *httpHandler) deleteUser(c *gin.Context) {
	version, err := ifMatchVersion(c)
	if err != nil {
		writeError(c, err)
		return
	}
	if err := h.store.DeleteUser(c.Param("id"), version); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ---- groups ----

func (h *httpHandler) createGroup(c *gin.Context) {
	var input Group
	if !decodeJSON(c, &input) {
		return
	}
	group, err := h.store.CreateGroup(baseURL(c), &input)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Location", group.Meta.Location)
	c.Header("ETag", group.Meta.Version)
	writeSCIM(c, http.StatusCreated, group)
}

func (h *httpHandler) getGroup(c *gin.Context) {
	group, err := h.store.GetGroup(baseURL(c), c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("ETag", group.Meta.Version)
	writeSCIM(c, http.StatusOK, group)
}

func (h *httpHandler) listGroups(c *gin.Context) {
	predicate, startIndex, count, ok := listArguments(c, ResourceTypeGroup)
	if !ok {
		return
	}
	groups, total, err := h.store.ListGroups(baseURL(c), predicate, startIndex, count)
	if err != nil {
		writeError(c, err)
		return
	}
	writeSCIM(c, http.StatusOK, ListResponse[*Group]{
		Schemas:      []string{ListResponseURI},
		TotalResults: total,
		ItemsPerPage: len(groups),
		StartIndex:   startIndex,
		Resources:    groups,
	})
}

func (h *httpHandler) replaceGroup(c *gin.Context) {
	version, err := ifMatchVersion(c)
	if err != nil {
		writeError(c, err)
		return
	}
	var input Group
	if !decodeJSON(c, &input) {
		return
	}
	group, err := h.store.ReplaceGroup(baseURL(c), c.Param("id"), &input, version)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("ETag", group.Meta.Version)
	writeSCIM(c, http.StatusOK, group)
}

func (h *httpHandler) patchGroup(c *gin.Context) {
	version, err := ifMatchVersion(c)
	if err != nil {
		writeError(c, err)
		return
	}
	var patch PatchOp
	if !decodeJSON(c, &patch) {
		return
	}
	if len(patch.Operations) == 0 {
		abortSCIM(c, http.StatusBadRequest, ScimTypeInvalidSyntax, "Operations must contain at least one entry")
		return
	}
	group, err := h.store.PatchGroup(baseURL(c), c.Param("id"), &patch, version)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("ETag", group.Meta.Version)
	writeSCIM(c, http.StatusOK, group)
}

func (h *httpHandler) deleteGroup(c *gin.Context) {
	version, err := ifMatchVersion(c)
	if err != nil {
		writeError(c, err)
		return
	}
	if err := h.store.DeleteGroup(c.Param("id"), version); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func listArguments(c *gin.Context, resourceType string) (goqu.Expression, int, int, bool) {
	startIndex := 1
	if raw := c.Query("startIndex"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			abortSCIM(c, http.StatusBadRequest, ScimTypeInvalidValue, "startIndex must be a positive integer")
			return nil, 0, 0, false
		}
		startIndex = parsed
	}
	count := DefaultCount
	if raw := c.Query("count"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			abortSCIM(c, http.StatusBadRequest, ScimTypeInvalidValue, "count must be a non-negative integer")
			return nil, 0, 0, false
		}
		count = parsed
		if count > maxFilterResults {
			count = maxFilterResults
		}
	}
	var predicate goqu.Expression
	if rawFilter := c.Query("filter"); rawFilter != "" {
		parsed, err := ParseFilter(resourceType, rawFilter)
		if err != nil {
			abortSCIM(c, http.StatusBadRequest, ScimTypeInvalidFilter, err.Error())
			return nil, 0, 0, false
		}
		predicate = parsed
	}
	return predicate, startIndex, count, true
}
