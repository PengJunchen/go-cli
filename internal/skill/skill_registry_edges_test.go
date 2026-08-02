package skill_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/skill"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// Get on a brand-new registry reports "not present" for any name, including
// an empty name. No skill exists yet.
func TestRegistryGetMissingOnEmpty(t *testing.T) {
	reg := skill.NewDefaultSkillRegistry()
	ctx := context.Background()

	_, ok := reg.Get(ctx, "anything")
	assert.False(t, ok)

	_, ok = reg.Get(ctx, "")
	assert.False(t, ok)

	assert.Empty(t, reg.List(ctx))
}

// Register of a typed nil interface is rejected with ErrNilSkill.
func TestRegistryRegisterNilInterfaceSkill(t *testing.T) {
	reg := skill.NewDefaultSkillRegistry()
	var def skill.SkillDefinition
	require.Nil(t, def)
	err := reg.Register(context.Background(), def)
	require.ErrorIs(t, err, skill.ErrNilSkill)
}

// Overwriting a name with a different category updates the category cache so
// the old category no longer lists the skill while the new one does.
func TestRegistryOverwriteChangesCategoryIndex(t *testing.T) {
	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()

	require.NoError(t, reg.Register(ctx, skill.NewSkill("s", skill.WithCategory("coding"))))
	require.NoError(t, reg.Register(ctx, skill.NewSkill("s", skill.WithCategory("writing"))))

	// Same slot in the global order, still a single entry.
	assert.Len(t, reg.List(ctx), 1)

	// Newest category is authoritative for List filtering.
	old := reg.List(ctx, "coding")
	assert.Empty(t, old)
	newer := reg.List(ctx, "writing")
	require.Len(t, newer, 1)
	assert.Equal(t, "writing", newer[0].Category())
}

// Re-registering an existing name preserves its original registration slot,
// so List order is stable across overwrites.
func TestRegistryReRegisterKeepsPosition(t *testing.T) {
	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()
	require.NoError(t, reg.Register(ctx, skill.NewSkill("alpha", skill.WithDescription("v1"))))
	require.NoError(t, reg.Register(ctx, skill.NewSkill("beta")))
	require.NoError(t, reg.Register(ctx, skill.NewSkill("gamma")))

	// Overwrite alpha; order must remain alpha, beta, gamma.
	require.NoError(t, reg.Register(ctx, skill.NewSkill("alpha", skill.WithDescription("v2"))))

	got := reg.List(ctx)
	require.Len(t, got, 3)
	assert.Equal(t, []string{"alpha", "beta", "gamma"},
		[]string{got[0].Name(), got[1].Name(), got[2].Name()})

	// The overwrite took effect on the stored definition.
	alpha, ok := reg.Get(ctx, "alpha")
	require.True(t, ok)
	assert.Equal(t, "v2", alpha.Description())
}

// Unregistering the middle entry preserves the relative order of the rest.
func TestRegistryUnregisterMiddlePreservesOrder(t *testing.T) {
	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()
	require.NoError(t, reg.Register(ctx, skill.NewSkill("one")))
	require.NoError(t, reg.Register(ctx, skill.NewSkill("two")))
	require.NoError(t, reg.Register(ctx, skill.NewSkill("three")))

	require.NoError(t, reg.Unregister(ctx, "two"))

	got := reg.List(ctx)
	require.Len(t, got, 2)
	assert.Equal(t, "one", got[0].Name())
	assert.Equal(t, "three", got[1].Name())
}

// A skill registered without a category can still be unregistered cleanly and
// the empty-category cache stays consistent.
func TestRegistryUnregisterNoCategory(t *testing.T) {
	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()
	require.NoError(t, reg.Register(ctx, skill.NewSkill("loner")))
	require.NoError(t, reg.Unregister(ctx, "loner"))

	_, ok := reg.Get(ctx, "loner")
	assert.False(t, ok)
	assert.Empty(t, reg.List(ctx))

	// Unregistering again surfaces ErrSkillNotFound.
	require.ErrorIs(t, reg.Unregister(ctx, "loner"), skill.ErrSkillNotFound)
}

// List with a category that matches nothing returns an empty result (nil is
// allowed — treat it as empty), and never a panic.
func TestRegistryListCategoryNoMatch(t *testing.T) {
	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()
	require.NoError(t, reg.Register(ctx, skill.NewSkill("a", skill.WithCategory("coding"))))

	got := reg.List(ctx, "music")
	assert.Empty(t, got)
}

// Skills without a declared category are returned when the empty string is
// used as the category filter.
func TestRegistryListCategoryEmptySelector(t *testing.T) {
	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()
	require.NoError(t, reg.Register(ctx, skill.NewSkill("uncategorized")))
	require.NoError(t, reg.Register(ctx, skill.NewSkill("categorized", skill.WithCategory("coding"))))

	got := reg.List(ctx, "")
	require.Len(t, got, 1)
	assert.Equal(t, "uncategorized", got[0].Name())
}

// Match is case-insensitive and ranks exact > prefix > substring on the name,
// while a description-only match scores lower still.
func TestRegistryMatchCaseInsensitiveRanking(t *testing.T) {
	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()
	require.NoError(t, reg.Register(ctx, skill.NewSkill("Deploy-App", skill.WithDescription("shipping"))))
	require.NoError(t, reg.Register(ctx, skill.NewSkill("deploy-script", skill.WithDescription("run deploy"))))

	got := reg.Match(ctx, "deploy")
	require.Len(t, got, 2)
	// "Deploy-App" (a prefix match, score 4) should outrank "deploy-script"
	// (substring match, score 3) despite the case difference.
	assert.Equal(t, "Deploy-App", got[0].Name())
	assert.Equal(t, "deploy-script", got[1].Name())
}

// Match surfaces a skill purely via its trigger hint when the description is
// empty and the name does not match.
func TestRegistryMatchTriggerHintOnly(t *testing.T) {
	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()
	require.NoError(t, reg.Register(ctx, skill.NewSkill("code-fix", skill.WithTriggerHint("repair the bug"))))

	got := reg.Match(ctx, "repair")
	require.Len(t, got, 1)
	assert.Equal(t, "code-fix", got[0].Name())
}

// Match ranks an exact name match above a description substring match even
// when more than one skill is present.
func TestRegistryMatchExactNameBeatsDescription(t *testing.T) {
	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()
	require.NoError(t, reg.Register(ctx, skill.NewSkill("lint", skill.WithDescription("static analysis"))))
	require.NoError(t, reg.Register(ctx, skill.NewSkill("go-lint", skill.WithDescription("lint go code"))))

	got := reg.Match(ctx, "lint")
	require.Len(t, got, 2)
	// "lint" is an exact name match (score 5) and must be first even though
	// "go-lint" also matches on both name and description.
	assert.Equal(t, "lint", got[0].Name())
}

// Match returns nil (not an empty slice) for a blank hint on a non-empty
// registry and is stable across repeated calls.
func TestRegistryMatchBlankHintNil(t *testing.T) {
	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()
	require.NoError(t, reg.Register(ctx, skill.NewSkill("a")))

	got := reg.Match(ctx, "\t  ")
	assert.Nil(t, got)
	got = reg.Match(ctx, "")
	assert.Nil(t, got)
}

// Many goroutines registering the same name concurrently must not corrupt the
// global order — the name appears exactly once in List.
func TestRegistryConcurrentRegisterSameName(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()

	const workers = 16
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			require.NoError(t, reg.Register(ctx, skill.NewSkill(
				"shared",
				skill.WithCategory("cat"),
				skill.WithDescription("writer"),
			)))
		}(i)
	}
	wg.Wait()

	got := reg.List(ctx)
	require.Len(t, got, 1)
	assert.Equal(t, "shared", got[0].Name())
}

// Concurrently registering distinct categories and filtering by each category
// must reflect every registered skill exactly once.
func TestRegistryConcurrentCategoryList(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('a' + i))
			cat := "group"
			if i%2 == 0 {
				cat = "even"
			}
			require.NoError(t, reg.Register(ctx, skill.NewSkill(name, skill.WithCategory(cat))))
		}(i)
	}
	wg.Wait()

	assert.Len(t, reg.List(ctx), 8)
	// The "group" category gets the four odd ones.
	assert.Len(t, reg.List(ctx, "group"), 4)
	assert.Len(t, reg.List(ctx, "even"), 4)
}

// A nil-def Register under concurrency returns ErrNilSkill and leaves the
// registry empty.
func TestRegistryConcurrentNilRegister(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = reg.Register(ctx, nil) //nolint:errcheck // asserting concurrent rejection
		}()
	}
	wg.Wait()
	assert.Empty(t, reg.List(ctx))
}
