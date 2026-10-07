package mcp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolve_ProjectPopulated(t *testing.T) {
	databases := []databaseEntry{
		{
			Name:    "instances/prod-pg/databases/employee_db",
			Project: "projects/hr-system",
			InstanceResource: instanceResource{
				Name:        "instances/prod-pg",
				Engine:      "POSTGRES",
				DataSources: []dataSource{{ID: "ds-admin-1", Type: "ADMIN"}},
			},
		},
	}

	resolved, err := matchDatabases(databases, "employee_db", "", "")
	require.NoError(t, err)
	require.False(t, resolved.ambiguous)
	require.Equal(t, "projects/hr-system", resolved.project)
	require.Equal(t, "instances/prod-pg/databases/employee_db", resolved.resourceName)
	require.Equal(t, "POSTGRES", resolved.engine)
}

func TestResolve_ProjectInAmbiguous(t *testing.T) {
	databases := []databaseEntry{
		{
			Name:    "instances/prod-pg/databases/app",
			Project: "projects/payments",
			InstanceResource: instanceResource{
				Name:        "instances/prod-pg",
				Engine:      "POSTGRES",
				DataSources: []dataSource{{ID: "ds-1", Type: "ADMIN"}},
			},
		},
		{
			Name:    "instances/staging-pg/databases/app",
			Project: "projects/staging",
			InstanceResource: instanceResource{
				Name:        "instances/staging-pg",
				Engine:      "POSTGRES",
				DataSources: []dataSource{{ID: "ds-2", Type: "ADMIN"}},
			},
		},
	}

	resolved, err := matchDatabases(databases, "app", "", "")
	require.NoError(t, err)
	require.True(t, resolved.ambiguous)
	require.Len(t, resolved.candidates, 2)
	require.Equal(t, "projects/payments", resolved.projects["instances/prod-pg/databases/app"])
	require.Equal(t, "projects/staging", resolved.projects["instances/staging-pg/databases/app"])
}

type capturedListCall struct {
	path   string
	parent string
}

func captureListCalls(t *testing.T, respond func(w http.ResponseWriter, call capturedListCall)) (*Server, *[]capturedListCall) {
	t.Helper()
	var calls []capturedListCall
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody struct {
			Parent string `json:"parent"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		call := capturedListCall{path: r.URL.Path, parent: reqBody.Parent}
		calls = append(calls, call)
		w.Header().Set("Content-Type", "application/json")
		respond(w, call)
	})
	return newTestServerWithMock(t, handler), &calls
}

func TestListDatabases_UsesProjectParent(t *testing.T) {
	s, calls := captureListCalls(t, func(w http.ResponseWriter, call capturedListCall) {
		require.Contains(t, call.path, "DatabaseService/ListDatabases")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"databases": []map[string]any{
				makeDatabase("instances/prod-pg/databases/employee_db", "instances/prod-pg", "projects/hr-system", "POSTGRES", "ds-admin-1"),
			},
		})
	})

	databases, err := s.listDatabases(testContext(), `name.contains("employee_db")`, "hr-system")
	require.NoError(t, err)
	require.Len(t, databases, 1)
	require.Equal(t, []capturedListCall{{
		path:   "/bytebase.v1.DatabaseService/ListDatabases",
		parent: "projects/hr-system",
	}}, *calls)
}

func TestListDatabases_WorkspaceForbiddenFallsBackToProjects(t *testing.T) {
	s, calls := captureListCalls(t, func(w http.ResponseWriter, call capturedListCall) {
		switch {
		case strings.Contains(call.path, "DatabaseService/ListDatabases") && strings.HasPrefix(call.parent, "workspaces/"):
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "permission denied", "code": "PERMISSION_DENIED"})
		case strings.Contains(call.path, "ProjectService/SearchProjects"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"projects": []map[string]any{
					{"name": "projects/hr-system"},
					{"name": "projects/commerce"},
				},
			})
		case strings.Contains(call.path, "DatabaseService/ListDatabases") && strings.HasPrefix(call.parent, "projects/"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"databases": []map[string]any{
					makeDatabase("instances/prod-pg/databases/app", "instances/prod-pg", call.parent, "POSTGRES", "ds-1"),
				},
			})
		default:
			t.Errorf("unexpected request path %s parent %s", call.path, call.parent)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	databases, err := s.listDatabases(testContext(), `name.contains("app")`, "")
	require.NoError(t, err)
	require.Len(t, databases, 2)
	require.Equal(t, "projects/hr-system", databases[0].Project)
	require.Equal(t, "projects/commerce", databases[1].Project)
	require.Equal(t, []capturedListCall{
		{path: "/bytebase.v1.DatabaseService/ListDatabases", parent: "workspaces/wk-test"},
		{path: "/bytebase.v1.ProjectService/SearchProjects", parent: ""},
		{path: "/bytebase.v1.DatabaseService/ListDatabases", parent: "projects/hr-system"},
		{path: "/bytebase.v1.DatabaseService/ListDatabases", parent: "projects/commerce"},
	}, *calls)
}

func TestListDatabases_WorkspaceSuccessSkipsFallback(t *testing.T) {
	s, calls := captureListCalls(t, func(w http.ResponseWriter, call capturedListCall) {
		require.Contains(t, call.path, "DatabaseService/ListDatabases")
		require.True(t, strings.HasPrefix(call.parent, "workspaces/"))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"databases": []map[string]any{
				makeDatabase("instances/prod-pg/databases/employee_db", "instances/prod-pg", "projects/hr-system", "POSTGRES", "ds-admin-1"),
			},
		})
	})

	databases, err := s.listDatabases(testContext(), `name.contains("employee_db")`, "")
	require.NoError(t, err)
	require.Len(t, databases, 1)
	require.Equal(t, []capturedListCall{{
		path:   "/bytebase.v1.DatabaseService/ListDatabases",
		parent: "workspaces/wk-test",
	}}, *calls)
}
