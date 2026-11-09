package config

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const disabledSkillsKey = "options.disabled_skills"

// SetSkillDisabled adds name to, or removes it from, the disabled_skills
// list in the given scope's config file. Only that file's own list is
// edited, so entries contributed by other config layers are never copied
// into it. The in-memory config is updated and then reloaded from disk so
// the merged list reflects every layer; a skill disabled by another file
// stays disabled after enabling it here.
func (s *ConfigStore) SetSkillDisabled(scope Scope, name string, disabled bool) error {
	if name == "" {
		return errors.New("skill name is required")
	}
	err := s.atomicWrite(scope, func(data []byte) ([]byte, error) {
		var list []string
		for _, v := range gjson.GetBytes(data, disabledSkillsKey).Array() {
			list = append(list, v.String())
		}
		return sjson.SetBytes(data, disabledSkillsKey, toggleSkillName(list, name, disabled))
	})
	if err != nil {
		return err
	}
	s.mutateInMemory(func(c *Config) {
		if c.Options == nil {
			c.Options = &Options{}
		}
		c.Options.DisabledSkills = toggleSkillName(c.Options.DisabledSkills, name, disabled)
	})
	if err := s.autoReload(context.Background()); err != nil {
		slog.Warn("Config file updated but failed to reload in-memory state", "error", err)
	}
	return nil
}

type localSkillOverrides struct {
	disabled []string
	enabled  []string
}

// SetLocalSkillOverrides records the repository-scoped skill overrides
// (from the project database) that EffectiveDisabledSkills applies on top
// of the config's disabled_skills.
func (s *ConfigStore) SetLocalSkillOverrides(disabled, enabled []string) {
	s.localSkillsMu.Lock()
	defer s.localSkillsMu.Unlock()
	s.localSkills = localSkillOverrides{
		disabled: slices.Clone(disabled),
		enabled:  slices.Clone(enabled),
	}
}

// EffectiveDisabledSkills returns the skills that are off for this
// workspace: the config's disabled_skills with the repository overrides
// applied. Every consumer that hides or filters skills uses this.
func (s *ConfigStore) EffectiveDisabledSkills() []string {
	var global []string
	if cfg := s.Config(); cfg != nil && cfg.Options != nil {
		global = cfg.Options.DisabledSkills
	}
	s.localSkillsMu.RLock()
	defer s.localSkillsMu.RUnlock()
	return EffectiveDisabledSkills(global, s.localSkills.disabled, s.localSkills.enabled)
}

// EffectiveDisabledSkills merges the config's disabled skills with the
// repository-scoped overrides, mirroring MCP toggles: a local override wins
// over the config in both directions. A skill is off when it is disabled
// locally, or disabled in the config without a local enable. The result is
// sorted and never nil.
func EffectiveDisabledSkills(global, localDisabled, localEnabled []string) []string {
	out := []string{}
	for _, name := range global {
		if !slices.Contains(localEnabled, name) {
			out = append(out, name)
		}
	}
	out = append(out, localDisabled...)
	slices.Sort(out)
	return slices.Compact(out)
}

// toggleSkillName returns a copy of list with name present (disabled) or
// absent (enabled). It never mutates list.
func toggleSkillName(list []string, name string, disabled bool) []string {
	out := slices.DeleteFunc(slices.Clone(list), func(n string) bool { return n == name })
	if disabled {
		out = append(out, name)
	}
	if out == nil {
		out = []string{}
	}
	return out
}
