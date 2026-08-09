package llm

import "errors"

// errDefaultModel reports that the default model is not backed by a real
// implementation.
var errDefaultModel = errors.New("llm: default model not implemented")
