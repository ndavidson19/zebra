package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/julienschmidt/httprouter"
	"github.com/project-safari/zebra"
	"github.com/project-safari/zebra/store"
)

type ResourceAPI struct {
	factory zebra.ResourceFactory
	Store   zebra.Store
}

type QueryRequest struct {
	IDs        []string      `json:"ids,omitempty"`
	Types      []string      `json:"types,omitempty"`
	Labels     []zebra.Query `json:"labels,omitempty"`
	Properties []zebra.Query `json:"properties,omitempty"`
}

var ErrQueryRequest = errors.New("invalid GET query request body")

func handleQuery(ctx context.Context, api *ResourceAPI) httprouter.Handle {
	return func(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
		qr := new(QueryRequest)

		// Read request, return error if applicable
		if err := readReq(ctx, req, qr); err != nil {
			res.WriteHeader(http.StatusBadRequest)

			return
		}

		// Validate query request and label/property queries
		if err := qr.Validate(ctx); err != nil {
			res.WriteHeader(http.StatusBadRequest)

			return
		}

		var resources *zebra.ResourceMap

		// Get resources based on primary key (ID, Type, or Label)
		switch {
		case len(qr.IDs) != 0:
			resources = api.Store.QueryUUID(qr.IDs)
		case len(qr.Types) != 0:
			resources = api.Store.QueryType(qr.Types)
		case len(qr.Labels) != 0:
			q := qr.Labels[0]
			qr.Labels = qr.Labels[1:]
			// Can safely ignore error because we have already validated the query
			resources, _ = api.Store.QueryLabel(q)
		default:
			resources = api.Store.Query()
		}

		// Filter further based on label queries
		for _, q := range qr.Labels {
			// Can safely ignore error because we have already validated the query
			resources, _ = store.FilterLabel(q, resources)
		}

		// Write response body
		writeJSON(ctx, res, resources)
	}
}

func (qr *QueryRequest) Validate(ctx context.Context) error {
	id := len(qr.IDs) != 0
	t := len(qr.Types) != 0
	l := len(qr.Labels) != 0
	p := len(qr.Properties) != 0

	// Make sure only id (and labels), types (and labels), or labels are present
	if (id && t) || (id && p) || (t && p) || (l && p) {
		return ErrQueryRequest
	}

	// Check Labels queries are valid
	if err := checkQueries(qr.Labels); err != nil {
		return err
	}

	// Check Properties queries are valid
	return checkQueries(qr.Properties)
}

func checkQueries(queries []zebra.Query) error {
	for _, q := range queries {
		if err := q.Validate(); err != nil {
			return err
		}
	}

	return nil
}

func NewResourceAPI(factory zebra.ResourceFactory) *ResourceAPI {
	return &ResourceAPI{
		factory: factory,
		Store:   nil,
	}
}

// Set up store and query store given storage root.
func (api *ResourceAPI) Initialize(storageRoot string) error {
	api.Store = store.NewResourceStore(storageRoot, api.factory)

	return api.Store.Initialize()
}

func (api *ResourceAPI) PutResource(w http.ResponseWriter, req *http.Request) {
	if req.Body == nil {
		w.WriteHeader(http.StatusBadRequest)

		return
	}

	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)

		return
	}

	res := api.unpackResource(w, body)
	if res == nil {
		w.WriteHeader(http.StatusBadRequest)

		return
	}

	// Check if this is a create or an update.
	exists := len(api.Store.QueryUUID([]string{res.GetID()}).Resources) != 0

	if err := api.Store.Create(res); err != nil {
		w.WriteHeader(http.StatusBadRequest)

		return
	}

	if exists {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func (api *ResourceAPI) DeleteResource(w http.ResponseWriter, req *http.Request) {
	if req.Body == nil {
		w.WriteHeader(http.StatusBadRequest)

		return
	}

	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)

		return
	}

	var ids []string

	err = json.Unmarshal(body, &ids)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)

		return
	}

	resources := api.Store.QueryUUID(ids)
	status := make(map[string]int, len(ids))

	for _, l := range resources.Resources {
		for _, res := range l.Resources {
			if api.Store.Delete(res) != nil {
				status[res.GetID()] = -1
			} else {
				status[res.GetID()] = 1
			}
		}
	}

	httpStatus, response := createDeleteResponse(ids, status)

	w.WriteHeader(httpStatus)
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(html.EscapeString(response))) //nolint:errcheck
}

func createDeleteResponse(ids []string, status map[string]int) (int, string) {
	httpStatus := http.StatusOK
	successID := make([]string, 0)
	failedID := make([]string, 0)
	invalidID := make([]string, 0)

	for _, id := range ids {
		switch status[id] {
		case -1:
			failedID = append(failedID, id)
		case 1:
			successID = append(successID, id)
		default:
			invalidID = append(invalidID, id)
		}
	}

	var response string

	if len(successID) > 0 {
		response = fmt.Sprintf("Deleted the following resources: %s\n", strings.Join(successID, ", "))
	}

	if len(failedID) > 0 {
		httpStatus = http.StatusMultiStatus

		response += fmt.Sprintf("Failed to delete the following resources: %s\n", strings.Join(failedID, ", "))
	}

	if len(invalidID) > 0 {
		response += fmt.Sprintf("Invalid resource IDs: %s\n", strings.Join(invalidID, ", "))
	}

	return httpStatus, response
}

func (api *ResourceAPI) unpackResource(w http.ResponseWriter, body []byte) zebra.Resource {
	object := make(map[string]interface{})
	if err := json.Unmarshal(body, &object); err != nil {
		return nil
	}

	resType, ok := object["type"].(string)
	if !ok {
		return nil
	}

	res := api.factory.New(resType)
	if err := json.Unmarshal(body, res); err != nil {
		return nil
	}

	return res
}