package llm_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestDispatchCapabilitiesAreLLMOwnedConcreteTypes(t *testing.T) {
	permitType := reflect.TypeOf(llm.DispatchPermit{})
	attemptType := reflect.TypeOf(llm.ProviderAttempt{})

	require.Equal(t, reflect.Struct, permitType.Kind(), "callers must not implement or mint permits")
	require.Equal(t, reflect.Struct, attemptType.Kind(), "callers must not implement or mint provider attempts")
	for _, typ := range []reflect.Type{permitType, attemptType} {
		for i := 0; i < typ.NumField(); i++ {
			require.False(t, typ.Field(i).IsExported(), "%s field %s must remain private", typ.Name(), typ.Field(i).Name)
		}
	}
	permit := llm.DispatchPermit{}
	attempt := llm.ProviderAttempt{}
	require.Empty(t, permit.OperationID(), "the exported zero value must not authorize an operation")
	require.Empty(t, permit.CandidateID(), "the exported zero value must not authorize a candidate")
	require.Empty(t, attempt.ID(), "the exported zero value must not identify an actual dispatch")
	_, err := attempt.Finish(llm.ProviderAttemptTerminal{})
	require.Error(t, err, "the exported zero value must not finish a provider attempt")
}
