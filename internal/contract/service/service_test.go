package service

import "github.com/rafaelp/vidyom/internal/contract/connector"

// The standard registry reports connectors directly.
var _ Connectors = (*connector.Registry)(nil)
