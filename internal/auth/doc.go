// Package auth defines authentication and authorization boundaries.
//
// Credential providers and policy decisions belong here; transport handlers and
// Kubernetes clients consume those decisions without owning secret material.
package auth
