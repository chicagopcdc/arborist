package arborist

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strings"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/uc-cdis/go-authutils/authutils"
)

type Server struct {
	db     *sqlx.DB
	jwtApp *authutils.JWTApplication
	logger *LogHandler
}

func NewServer() *Server {
	return &Server{}
}

func (server *Server) WithLogger(logger *log.Logger) *Server {
	server.logger = &LogHandler{logger: logger}
	return server
}

func (server *Server) WithJWTApp(jwtApp *authutils.JWTApplication) *Server {
	server.jwtApp = jwtApp
	return server
}

func (server *Server) WithDB(db *sqlx.DB) *Server {
	server.db = db
	return server
}

func (server *Server) Init() (*Server, error) {
	if server.db == nil {
		return nil, errors.New("arborist server initialized without database")
	}
	if server.jwtApp == nil {
		return nil, errors.New("arborist server initialized without JWT app")
	}
	if server.logger == nil {
		return nil, errors.New("arborist server initialized without logger")
	}

	return server, nil
}

// For some reason this is not allowed:
//
//    `{resourcePath:/[a-zA-Z0-9_\-\/]+}`
//
// so we put the slash at the front here and fix it in parseResourcePath.
const resourcePath string = `/{resourcePath:[a-zA-Z0-9_\-\/]+}`

func parseResourcePath(r *http.Request) string {
	path, exists := mux.Vars(r)["resourcePath"]
	if !exists {
		// should never happen: route was set up to call this function when the
		// URL did not actually match a resource path
		panic(errors.New("fix resource routes"))
	}
	// We have to add a slash at the front here; see resourcePath constant.
	return strings.Join([]string{"/", path}, "")
}

func (server *Server) MakeRouter(out io.Writer) http.Handler {
	router := mux.NewRouter().StrictSlash(false)

	//router.Handle("/", server.handleRoot).Methods("GET")

	router.HandleFunc("/health", server.handleHealth).Methods("GET")

	router.Handle("/auth/proxy", http.HandlerFunc(server.handleAuthProxy)).Methods("GET")
	router.Handle("/auth/request", http.HandlerFunc(parseJSON(server.handleAuthRequest))).Methods("POST")
	//router.Handle("/auth/resources", server.handleListAuthResources).Methods("POST")

	router.Handle("/policy", http.HandlerFunc(server.handlePolicyList)).Methods("GET")
	router.Handle("/policy", http.HandlerFunc(parseJSON(server.handlePolicyCreate))).Methods("POST")
	router.Handle("/policy/{policyID}", http.HandlerFunc(server.handlePolicyRead)).Methods("GET")
	router.Handle("/policy/{policyID}", http.HandlerFunc(server.handlePolicyDelete)).Methods("DELETE")

	router.Handle("/resource", http.HandlerFunc(server.handleResourceList)).Methods("GET")
	router.Handle("/resource", http.HandlerFunc(parseJSON(server.handleResourceCreate))).Methods("POST")
	router.Handle("/resource"+resourcePath, http.HandlerFunc(server.handleResourceRead)).Methods("GET")
	router.Handle("/resource"+resourcePath, http.HandlerFunc(parseJSON(server.handleSubresourceCreate))).Methods("POST")
	router.Handle("/resource"+resourcePath, http.HandlerFunc(server.handleResourceDelete)).Methods("DELETE")

	router.Handle("/role", http.HandlerFunc(server.handleRoleList)).Methods("GET")
	router.Handle("/role", http.HandlerFunc(parseJSON(server.handleRoleCreate))).Methods("POST")
	router.Handle("/role/{roleID}", http.HandlerFunc(server.handleRoleRead)).Methods("GET")
	router.Handle("/role/{roleID}", http.HandlerFunc(server.handleRoleDelete)).Methods("DELETE")

	return handlers.CombinedLoggingHandler(out, router)
}

// parseJSON abstracts JSON parsing for handler functions that should
// receive a valid JSON input in the request body. It takes a modified
// handler function as input, which should include the body in `[]byte`
// form as an additional argument, and returns a function with the usual
// handler signature.
func parseJSON(baseHandler func(http.ResponseWriter, *http.Request, []byte)) func(http.ResponseWriter, *http.Request) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			msg := fmt.Sprintf("could not parse valid JSON from request: %s", err.Error())
			response := newErrorResponse(msg, 400, nil)
			_ = response.write(w, r)
			return
		}
		baseHandler(w, r, body)
	}
	return handler
}

func (server *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	err := server.db.Ping()
	if err != nil {
		server.logger.Error("database ping failed; returning unhealthy")
		response := newErrorResponse("database unavailable", 500, nil)
		_ = response.write(w, r)
	}
	w.WriteHeader(http.StatusOK)
}

func (server *Server) handleAuthProxy(w http.ResponseWriter, r *http.Request) {
	// Get QS arguments
	resourcePathQS, ok := r.URL.Query()["resource"]
	if !ok {
		msg := "auth proxy request missing `resource` argument"
		server.logger.Info(msg)
		errResponse := newErrorResponse(msg, 400, nil)
		_ = errResponse.write(w, r)
		return
	}
	resourcePath := resourcePathQS[0]
	serviceQS, ok := r.URL.Query()["service"]
	if !ok {
		msg := "auth proxy request missing `service` argument"
		server.logger.Info(msg)
		errResponse := newErrorResponse(msg, 400, nil)
		_ = errResponse.write(w, r)
		return
	}
	service := serviceQS[0]
	methodQS, ok := r.URL.Query()["method"]
	if !ok {
		msg := "auth proxy request missing `method` argument"
		server.logger.Info(msg)
		errResponse := newErrorResponse(msg, 400, nil)
		_ = errResponse.write(w, r)
		return
	}
	method := methodQS[0]
	// get JWT from auth header and decode it
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		msg := "auth proxy request missing auth header"
		server.logger.Info(msg)
		errResponse := newErrorResponse(msg, 400, nil)
		_ = errResponse.write(w, r)
		return
	}
	userJWT := strings.TrimPrefix(authHeader, "Bearer ")
	userJWT = strings.TrimPrefix(userJWT, "bearer ")
	aud := []string{"openid"}
	info, err := server.decodeToken(userJWT, aud)
	if err != nil {
		server.logger.Info(err.Error())
		errResponse := newErrorResponse(err.Error(), 401, &err)
		_ = errResponse.write(w, r)
		return
	}

	w.Header().Set("REMOTE_USER", info.username)

	rv, err := authorize(server.db, info, resourcePath, service, method)
	if err != nil {
		msg := fmt.Sprintf("could not authorize: %s", err.Error())
		server.logger.Info("tried to handle auth request but input was invalid: %s", msg)
		response := newErrorResponse(msg, 400, nil)
		_ = response.write(w, r)
		return
	}
	if !rv {
		errResponse := newErrorResponse(
			"Unauthorized: user does not have access to this resource", 403, nil)
		_ = errResponse.write(w, r)
	}
}

func (server *Server) handleAuthRequest(w http.ResponseWriter, r *http.Request, body []byte) {
	authRequest := &AuthRequest{}
	err := json.Unmarshal(body, authRequest)
	if err != nil {
		msg := fmt.Sprintf("could not parse auth request from JSON: %s", err.Error())
		server.logger.Info("tried to handle auth request but input was invalid: %s", msg)
		response := newErrorResponse(msg, 400, nil)
		_ = response.write(w, r)
		return
	}

	var aud []string
	if authRequest.User.Audiences == nil {
		aud = []string{"openid"}
	} else {
		aud = make([]string, len(authRequest.User.Audiences))
		copy(aud, authRequest.User.Audiences)
	}

	info, err := server.decodeToken(authRequest.User.Token, aud)
	if err != nil {
		server.logger.Info(err.Error())
		errResponse := newErrorResponse(err.Error(), 401, &err)
		_ = errResponse.write(w, r)
		return
	}

	if authRequest.User.Policies != nil {
		info.policies = authRequest.User.Policies
	}

	rv, err := authorize(server.db, info, authRequest.Request.Resource,
		authRequest.Request.Action.Service, authRequest.Request.Action.Method)
	if err != nil {
		msg := fmt.Sprintf("could not authorize: %s", err.Error())
		server.logger.Info("tried to handle auth request but input was invalid: %s", msg)
		response := newErrorResponse(msg, 400, nil)
		_ = response.write(w, r)
		return
	}
	_ = jsonResponseFrom(AuthResponse{rv}, 200).write(w, r)
}

func (server *Server) handlePolicyList(w http.ResponseWriter, r *http.Request) {
	policies, err := listPoliciesFromDb(server.db)
	if err != nil {
		msg := fmt.Sprintf("policies query failed: %s", err.Error())
		errResponse := newErrorResponse(msg, 500, nil)
		server.logger.Error(errResponse.Error.Message)
		_ = errResponse.write(w, r)
		return
	}
	_ = jsonResponseFrom(policies, http.StatusOK).write(w, r)
}

func (server *Server) handlePolicyCreate(w http.ResponseWriter, r *http.Request, body []byte) {
	policy := &Policy{}
	err := json.Unmarshal(body, policy)
	if err != nil {
		msg := fmt.Sprintf("could not parse policy from JSON: %s", err.Error())
		server.logger.Info("tried to create policy but input was invalid: %s", msg)
		response := newErrorResponse(msg, 400, nil)
		_ = response.write(w, r)
		return
	}
	errResponse := policy.createInDb(server.db)
	if errResponse != nil {
		if errResponse.Error.Code >= 500 {
			server.logger.Error(errResponse.Error.Message)
		} else {
			server.logger.Info(errResponse.Error.Message)
		}
		_ = errResponse.write(w, r)
		return
	}
	created := struct {
		Created *Policy `json:"created"`
	}{
		Created: policy,
	}
	_ = jsonResponseFrom(created, 201).write(w, r)
}

func (server *Server) handlePolicyRead(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["policyID"]
	policyFromQuery, err := policyWithName(server.db, name)
	if policyFromQuery == nil {
		msg := fmt.Sprintf("no policy found with id: %s", name)
		errResponse := newErrorResponse(msg, 404, nil)
		server.logger.Error(errResponse.Error.Message)
		_ = errResponse.write(w, r)
		return
	}
	if err != nil {
		msg := fmt.Sprintf("policy query failed: %s", err.Error())
		errResponse := newErrorResponse(msg, 500, nil)
		server.logger.Error(errResponse.Error.Message)
		_ = errResponse.write(w, r)
		return
	}
	policy := policyFromQuery.standardize()
	_ = jsonResponseFrom(policy, http.StatusOK).write(w, r)
}

func (server *Server) handlePolicyDelete(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["policyID"]
	policy := &Policy{Name: name}
	errResponse := policy.deleteInDb(server.db)
	if errResponse != nil {
		server.logger.Info(errResponse.Error.Message)
		_ = errResponse.write(w, r)
		return
	}
	_ = jsonResponseFrom(nil, http.StatusCreated).write(w, r)
}

func (server *Server) handleResourceList(w http.ResponseWriter, r *http.Request) {
	resourcesFromQuery, err := listResourcesFromDb(server.db)
	resources := []*Resource{}
	for _, resourceFromQuery := range resourcesFromQuery {
		resources = append(resources, resourceFromQuery.standardize())
	}
	if err != nil {
		msg := fmt.Sprintf("resources query failed: %s", err.Error())
		errResponse := newErrorResponse(msg, 500, nil)
		server.logger.Error(errResponse.Error.Message)
		_ = errResponse.write(w, r)
		return
	}
	_ = jsonResponseFrom(resources, http.StatusOK).write(w, r)
}

func (server *Server) handleResourceCreate(w http.ResponseWriter, r *http.Request, body []byte) {
	resource := &Resource{}
	err := json.Unmarshal(body, resource)
	if err != nil {
		msg := fmt.Sprintf("could not parse resource from JSON: %s", err.Error())
		server.logger.Info("tried to create resource but input was invalid: %s", msg)
		response := newErrorResponse(msg, 400, nil)
		_ = response.write(w, r)
		return
	}
	if resource.Path == "" {
		err := missingRequiredField("resource", "path")
		server.logger.Info(err.Error())
		response := newErrorResponse(err.Error(), 400, &err)
		_ = response.write(w, r)
		return
	}
	errResponse := resource.createInDb(server.db)
	if errResponse != nil {
		if errResponse.Error.Code >= 500 {
			server.logger.Error(errResponse.Error.Message)
		} else {
			server.logger.Info(errResponse.Error.Message)
		}
		_ = errResponse.write(w, r)
		return
	}
	created := struct {
		Created *Resource `json:"created"`
	}{
		Created: resource,
	}
	_ = jsonResponseFrom(created, 201).write(w, r)
}

func (server *Server) handleSubresourceCreate(w http.ResponseWriter, r *http.Request, body []byte) {
	resource := &Resource{}
	err := json.Unmarshal(body, resource)
	if err != nil {
		msg := fmt.Sprintf("could not parse resource from JSON: %s", err.Error())
		server.logger.Info("tried to create resource but input was invalid: %s", msg)
		response := newErrorResponse(msg, 400, nil)
		_ = response.write(w, r)
		return
	}
	if resource.Name == "" {
		err := missingRequiredField("resource", "name")
		server.logger.Info(err.Error())
		response := newErrorResponse(err.Error(), 400, &err)
		_ = response.write(w, r)
		return
	}
	parentPath := parseResourcePath(r)
	resource.Path = parentPath + "/" + resource.Name
	errResponse := resource.createInDb(server.db)
	if errResponse != nil {
		if errResponse.Error.Code >= 500 {
			server.logger.Error(errResponse.Error.Message)
		} else {
			server.logger.Info(errResponse.Error.Message)
		}
		_ = errResponse.write(w, r)
		return
	}
	created := struct {
		Created *Resource `json:"created"`
	}{
		Created: resource,
	}
	_ = jsonResponseFrom(created, 201).write(w, r)
}

func (server *Server) handleResourceRead(w http.ResponseWriter, r *http.Request) {
	path := parseResourcePath(r)
	resourceFromQuery, err := resourceWithPath(server.db, path)
	if resourceFromQuery == nil {
		msg := fmt.Sprintf("no resource found with path: `%s`", path)
		errResponse := newErrorResponse(msg, 404, nil)
		_ = errResponse.write(w, r)
		return
	}
	if err != nil {
		msg := fmt.Sprintf("resource query failed: %s", err.Error())
		errResponse := newErrorResponse(msg, 500, nil)
		server.logger.Error(errResponse.Error.Message)
		_ = errResponse.write(w, r)
		return
	}
	resource := resourceFromQuery.standardize()
	_ = jsonResponseFrom(resource, http.StatusOK).write(w, r)
}

func (server *Server) handleResourceDelete(w http.ResponseWriter, r *http.Request) {
	path := parseResourcePath(r)
	resource := Resource{Path: path}
	errResponse := resource.deleteInDb(server.db)
	if errResponse != nil {
		server.logger.Info(errResponse.Error.Message)
		_ = errResponse.write(w, r)
		return
	}
	_ = jsonResponseFrom(nil, http.StatusNoContent).write(w, r)
}

func (server *Server) handleRoleList(w http.ResponseWriter, r *http.Request) {
	rolesFromQuery, err := listRolesFromDb(server.db)
	if err != nil {
		msg := fmt.Sprintf("roles query failed: %s", err.Error())
		errResponse := newErrorResponse(msg, 500, nil)
		server.logger.Error(errResponse.Error.Message)
		_ = errResponse.write(w, r)
		return
	}
	roles := []Role{}
	for _, roleFromQuery := range rolesFromQuery {
		roles = append(roles, roleFromQuery.standardize())
	}
	_ = jsonResponseFrom(roles, http.StatusOK).write(w, r)
}

func (server *Server) handleRoleCreate(w http.ResponseWriter, r *http.Request, body []byte) {
	role := &Role{}
	err := json.Unmarshal(body, role)
	if err != nil {
		msg := fmt.Sprintf("could not parse role from JSON: %s", err.Error())
		server.logger.Info("tried to create role but input was invalid: %s", msg)
		response := newErrorResponse(msg, 400, nil)
		_ = response.write(w, r)
		return
	}
	errResponse := role.createInDb(server.db)
	if errResponse != nil {
		if errResponse.Error.Code >= 500 {
			server.logger.Error(errResponse.Error.Message)
		} else {
			server.logger.Info(errResponse.Error.Message)
		}
		_ = errResponse.write(w, r)
		return
	}
	created := struct {
		Created *Role `json:"created"`
	}{
		Created: role,
	}
	_ = jsonResponseFrom(created, 201).write(w, r)
}

func (server *Server) handleRoleRead(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["roleID"]
	roleFromQuery, err := roleWithName(server.db, name)
	if roleFromQuery == nil {
		msg := fmt.Sprintf("no role found with id: %s", name)
		errResponse := newErrorResponse(msg, 404, nil)
		server.logger.Error(errResponse.Error.Message)
		_ = errResponse.write(w, r)
		return
	}
	if err != nil {
		msg := fmt.Sprintf("role query failed: %s", err.Error())
		errResponse := newErrorResponse(msg, 500, nil)
		server.logger.Error(errResponse.Error.Message)
		_ = errResponse.write(w, r)
		return
	}
	role := roleFromQuery.standardize()
	_ = jsonResponseFrom(role, http.StatusOK).write(w, r)
}

func (server *Server) handleRoleDelete(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["roleID"]
	role := &Role{Name: name}
	errResponse := role.deleteInDb(server.db)
	if errResponse != nil {
		server.logger.Info(errResponse.Error.Message)
		_ = errResponse.write(w, r)
		return
	}
	_ = jsonResponseFrom(nil, http.StatusNoContent).write(w, r)
}
