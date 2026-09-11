package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"strings"
)

// Resolve maps an incoming request onto one of the configured models.
//
// modelName is the value of the "model" field of the request body (it may be
// empty for requests that carry no model, such as DELETE /v1/responses/{id}).
// clientKey is the credential the client presented. The returned model holds
// both the upstream to talk to and the model name to request there.
func (c *Config) Resolve(modelName, clientKey string) (*Model, error) {
	modelName = strings.TrimSpace(modelName)

	if modelName != "" {
		matched := c.modelsByName(modelName)
		if len(matched) == 0 {
			return nil, &ResolveError{
				Status:  StatusNotFound,
				Code:    "model_not_found",
				Message: fmt.Sprintf("the model %q is not offered by this proxy", modelName),
			}
		}
		for _, m := range matched {
			if keyMatches(m.Key, clientKey) {
				return m, nil
			}
		}
		return nil, &ResolveError{
			Status:  StatusUnauthorized,
			Code:    "invalid_api_key",
			Message: fmt.Sprintf("the API key is not valid for the model %q", modelName),
		}
	}

	// No model in the request: fall back to the key, which must identify a
	// single mapping unambiguously.
	matched := c.modelsByKey(clientKey)
	switch len(matched) {
	case 0:
		return nil, &ResolveError{
			Status:  StatusUnauthorized,
			Code:    "invalid_api_key",
			Message: "the API key is not recognised by this proxy",
		}
	case 1:
		return matched[0], nil
	}
	first := matched[0]
	for _, m := range matched[1:] {
		if m.Upstream != first.Upstream || m.UpstreamModel != first.UpstreamModel {
			return nil, &ResolveError{
				Status: StatusBadRequest,
				Code:   "ambiguous_model",
				Message: "the API key maps to several upstream models; " +
					"specify the model in the request to disambiguate",
			}
		}
	}
	return first, nil
}

// KnownKey reports whether the key belongs to at least one configured model.
func (c *Config) KnownKey(clientKey string) bool {
	return len(c.modelsByKey(clientKey)) > 0
}

// ModelNames returns every model name this proxy offers, in configuration order.
func (c *Config) ModelNames() []string {
	names := make([]string, 0, len(c.Models))
	for i := range c.Models {
		names = append(names, c.Models[i].Name)
	}
	return names
}

func (c *Config) modelsByName(name string) []*Model {
	var out []*Model
	for i := range c.Models {
		if c.Models[i].Name == name {
			out = append(out, &c.Models[i])
		}
	}
	return out
}

func (c *Config) modelsByKey(key string) []*Model {
	var out []*Model
	if key == "" {
		return nil
	}
	for i := range c.Models {
		if keyMatches(c.Models[i].Key, key) {
			out = append(out, &c.Models[i])
		}
	}
	return out
}

// keyMatches compares two secrets in constant time, including their length.
func keyMatches(expected, got string) bool {
	if expected == "" {
		return false
	}
	expectedSum := sha256.Sum256([]byte(expected))
	gotSum := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(expectedSum[:], gotSum[:]) == 1
}
