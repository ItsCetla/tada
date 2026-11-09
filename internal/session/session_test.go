package session

import (
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestEstimatedUsageStateSurvivesFetchModifySave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.EstimatedUsage = true

	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.True(t, saved.EstimatedUsage)

	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, fetched.EstimatedUsage)

	fetched.Todos = []Todo{{
		Content:    "Check estimate state",
		Status:     TodoStatusInProgress,
		ActiveForm: "Checking estimate state",
	}}

	updated, err := sessions.Save(t.Context(), fetched)
	require.NoError(t, err)
	require.True(t, updated.EstimatedUsage)

	refetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, refetched.EstimatedUsage)
}

func TestSessionChannelPersists(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	// Release closes this test's own database. ResetPool would close the
	// databases of tests running in parallel too.
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "channel")
	require.NoError(t, err)
	updated, err := sessions.SetChannel(t.Context(), created.ID, "signal")
	require.NoError(t, err)
	require.Equal(t, "signal", updated.Channel)

	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "signal", fetched.Channel)
}

func TestSessionWorkingDirPersists(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	// Release closes this test's own database. ResetPool would close the
	// databases of tests running in parallel too.
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "folder")
	require.NoError(t, err)
	require.Empty(t, created.WorkingDir)

	dir := t.TempDir()
	updated, err := sessions.SetWorkingDir(t.Context(), created.ID, dir)
	require.NoError(t, err)
	require.Equal(t, dir, updated.WorkingDir)

	// A plain save must not drop the folder.
	updated.Title = "renamed"
	_, err = sessions.Save(t.Context(), updated)
	require.NoError(t, err)
	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, dir, fetched.WorkingDir)

	reset, err := sessions.SetWorkingDir(t.Context(), created.ID, "")
	require.NoError(t, err)
	require.Empty(t, reset.WorkingDir)
	fetched, err = sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Empty(t, fetched.WorkingDir)
}

func TestMCPServerDisabledRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	disabled, err := sessions.MCPDisabledServers(t.Context())
	require.NoError(t, err)
	require.Empty(t, disabled, "a new repository must default to the config")

	require.NoError(t, sessions.SetMCPServerDisabled(t.Context(), "docker", true))
	require.NoError(t, sessions.SetMCPServerDisabled(t.Context(), "serena", true))
	require.NoError(t, sessions.SetMCPServerDisabled(t.Context(), "docker", true), "disabling twice must be idempotent")

	disabled, err = sessions.MCPDisabledServers(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"docker", "serena"}, disabled)

	require.NoError(t, sessions.SetMCPServerDisabled(t.Context(), "docker", false))
	disabled, err = sessions.MCPDisabledServers(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"serena"}, disabled)

	// Enabling records an enabled override so a config-disabled server
	// stays enabled across restarts; disabling removes it again.
	enabled, err := sessions.MCPServersEnabled(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"docker"}, enabled)

	require.NoError(t, sessions.SetMCPServerDisabled(t.Context(), "docker", true))
	enabled, err = sessions.MCPServersEnabled(t.Context())
	require.NoError(t, err)
	require.Empty(t, enabled)
}

func TestSkillDisabledRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	disabled, err := sessions.SkillsDisabled(t.Context())
	require.NoError(t, err)
	require.Empty(t, disabled, "a new repository must default to the config")

	require.NoError(t, sessions.SetSkillDisabled(t.Context(), "jq", true))
	require.NoError(t, sessions.SetSkillDisabled(t.Context(), "jq", true), "disabling twice must be idempotent")
	disabled, err = sessions.SkillsDisabled(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"jq"}, disabled)

	// Enabling clears the disabled override and records an enabled one.
	require.NoError(t, sessions.SetSkillDisabled(t.Context(), "jq", false))
	disabled, err = sessions.SkillsDisabled(t.Context())
	require.NoError(t, err)
	require.Empty(t, disabled)
	enabled, err := sessions.SkillsEnabled(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"jq"}, enabled)

	// Disabling again clears the enabled override.
	require.NoError(t, sessions.SetSkillDisabled(t.Context(), "jq", true))
	enabled, err = sessions.SkillsEnabled(t.Context())
	require.NoError(t, err)
	require.Empty(t, enabled)
}

func TestEstimatedUsageStateCanBeClearedByExplicitSave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.EstimatedUsage = true

	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.True(t, saved.EstimatedUsage)

	saved.EstimatedUsage = false
	updated, err := sessions.Save(t.Context(), saved)
	require.NoError(t, err)
	require.False(t, updated.EstimatedUsage)

	refetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.False(t, refetched.EstimatedUsage)
}
