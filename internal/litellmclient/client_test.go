package litellmclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_ListModelsParsesDataAndAuth(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		assert.Equal(t, "/model/info", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"model_name":     "glm",
				"model_info":     map[string]any{"id": "abc", "managed_by": "litellm-operator"},
				"litellm_params": map[string]any{"model": "openai/glm"},
			}},
		})
	}))
	defer srv.Close()

	models, err := New(srv.URL, "sk-master", srv.Client()).ListModels(context.Background())
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, "glm", models[0].ModelName)
	assert.Equal(t, "abc", models[0].ModelID())
	assert.Equal(t, "Bearer sk-master", auth)
}

func TestClient_CreateUpdateDeleteSendCorrectRequests(t *testing.T) {
	type call struct {
		method, path string
		body         map[string]any
	}
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, call{r.Method, r.URL.Path, body})
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := New(srv.URL, "k", srv.Client())
	ctx := context.Background()

	require.NoError(t, c.CreateModel(ctx, Model{ModelName: "m", LiteLLMParams: map[string]any{"model": "openai/m"}}))
	require.NoError(t, c.UpdateModel(ctx, Model{ModelName: "m", ModelInfo: map[string]any{"id": "x"}}))
	require.NoError(t, c.DeleteModel(ctx, "x"))

	require.Len(t, calls, 3)
	assert.Equal(t, "/model/new", calls[0].path)
	assert.Equal(t, "openai/m", calls[0].body["litellm_params"].(map[string]any)["model"])
	assert.Equal(t, "/model/update", calls[1].path)
	assert.Equal(t, "/model/delete", calls[2].path)
	assert.Equal(t, "x", calls[2].body["id"])
}

func TestClient_GenerateAndDeleteVirtualKey(t *testing.T) {
	type call struct {
		path string
		body map[string]any
	}
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		calls = append(calls, call{path: r.URL.Path, body: body})
		if r.URL.Path == "/key/generate" {
			_ = json.NewEncoder(w).Encode(map[string]string{"key": "sk-generated"})
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "master", srv.Client())
	generated, err := c.GenerateVirtualKey(context.Background(), VirtualKeyRequest{
		KeyAlias: "application",
		Models:   []string{"openai/gpt-5"},
		Metadata: map[string]string{"app": "example"},
	})
	require.NoError(t, err)
	assert.Equal(t, "sk-generated", generated.Key)
	require.NoError(t, c.DeleteVirtualKey(context.Background(), generated.Key))

	require.Len(t, calls, 2)
	assert.Equal(t, "/key/generate", calls[0].path)
	assert.Equal(t, "application", calls[0].body["key_alias"])
	assert.Equal(t, []any{"openai/gpt-5"}, calls[0].body["models"])
	assert.Equal(t, "/key/delete", calls[1].path)
	assert.Equal(t, []any{"sk-generated"}, calls[1].body["keys"])
}

func TestClient_GetAndUpdateVirtualKey(t *testing.T) {
	var update map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/key/info" {
			assert.Equal(t, "sk-live", r.URL.Query().Get("key"))
			_ = json.NewEncoder(w).Encode(map[string]any{"key_alias": "application", "models": []string{"old"}})
			return
		}
		require.Equal(t, "/key/update", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&update))
	}))
	defer srv.Close()

	c := New(srv.URL, "master", srv.Client())
	live, err := c.GetVirtualKey(context.Background(), "sk-live")
	require.NoError(t, err)
	assert.Equal(t, "application", live.KeyAlias)
	require.NoError(t, c.UpdateVirtualKey(context.Background(), "sk-live", VirtualKeyRequest{Models: []string{"new"}}))
	assert.Equal(t, "sk-live", update["key"])
	assert.Equal(t, []any{"new"}, update["models"])
}

func TestClient_ManageTeam(t *testing.T) {
	const (
		teamID         = "platform"
		userID         = "user"
		teamMemberRole = "user"
	)
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/team/new" {
			_ = json.NewEncoder(w).Encode(map[string]string{"team_id": teamID})
		}
		if r.URL.Path == "/team/info" {
			_ = json.NewEncoder(w).Encode(map[string]any{"team_info": map[string]any{"team_id": teamID, "members_with_roles": []any{}}})
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "master", srv.Client())
	team, err := c.CreateTeam(context.Background(), TeamRequest{TeamID: teamID, Models: []string{}})
	require.NoError(t, err)
	assert.Equal(t, teamID, team.TeamID)
	require.NoError(t, c.UpdateTeam(context.Background(), TeamRequest{TeamID: team.TeamID, Models: []string{}}))
	_, err = c.GetTeam(context.Background(), team.TeamID)
	require.NoError(t, err)
	require.NoError(t, c.AddTeamMember(context.Background(), team.TeamID, TeamMember{UserID: userID, Role: teamMemberRole}))
	require.NoError(t, c.DeleteTeamMember(context.Background(), team.TeamID, TeamMember{UserID: userID}))
	require.NoError(t, c.DeleteTeam(context.Background(), team.TeamID))
	assert.Equal(t, []string{"/team/new", "/team/update", "/team/info", "/team/member_add", "/team/member_delete", "/team/delete"}, paths)
}

func TestClient_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()
	err := New(srv.URL, "k", srv.Client()).DeleteModel(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 500")
	assert.Contains(t, err.Error(), "boom")
}
