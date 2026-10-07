// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SchemaGrant declares the privileges a role holds on one schema.
type SchemaGrant struct {
	// Schema is the PostgreSQL schema the privileges apply to. With OwnerOf set
	// it is created if missing.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	Schema string `json:"schema"`

	// Privileges granted on the schema and the relations in it. USAGE and
	// CREATE apply to the schema (USAGE is always granted); the rest are granted
	// ON ALL TABLES, and the subset a sequence accepts (SELECT, UPDATE, USAGE)
	// ON ALL SEQUENCES.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Enum=SELECT;INSERT;UPDATE;DELETE;TRUNCATE;REFERENCES;TRIGGER;USAGE;CREATE
	Privileges []string `json:"privileges"`

	// OwnerOf makes the role the owner of the schema and of every table,
	// sequence, view and materialized view in it, so its migrations can alter
	// and replace them.
	// +optional
	OwnerOf bool `json:"ownerOf,omitempty"`
}

// InstanceRef identifies the PostgreSQL instance to act on: RDS, Aurora, or a
// self-managed PostgreSQL. Only the admin connection's authentication differs
// between them.
//
// +kubebuilder:validation:XValidation:rule="(has(self.auth) && has(self.auth.method) && self.auth.method == 'password') || (has(self.region) && size(self.region) > 0)",message="instance.region is required unless instance.auth.method is password"
type InstanceRef struct {
	// Endpoint is the instance's hostname.
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// Port is the PostgreSQL port.
	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`

	// Database is the database to connect to and grant CONNECT on. Single
	// hyphens between other characters are allowed; such a name must be
	// double-quoted in hand-written SQL.
	// +kubebuilder:default=appdb
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*(-[a-z0-9_]+)*$`
	// +optional
	Database string `json:"database,omitempty"`

	// Region is the AWS region the IAM auth token is signed for. Required for
	// IAM auth (the default); ignored under password auth.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Region string `json:"region,omitempty"`

	// AdminUser is the PostgreSQL role the controller connects as. It needs
	// CREATEROLE and enough membership to reassign ownership.
	// +kubebuilder:default=db_provisioner
	// +optional
	AdminUser string `json:"adminUser,omitempty"`

	// SSLMode is the libpq sslmode for the admin connection. verify-full (the
	// default) checks the certificate against the system CAs plus the built-in
	// RDS CAs and checks that it names the endpoint; verify-ca skips the name
	// check, for an endpoint CNAME. require encrypts without verifying, and
	// disable is for a local PostgreSQL without TLS.
	// +kubebuilder:validation:Enum=disable;require;verify-ca;verify-full
	// +kubebuilder:default=verify-full
	// +optional
	SSLMode string `json:"sslMode,omitempty"`

	// Auth is how the controller authenticates as AdminUser. Unrelated to
	// spec.grantRdsIam, which concerns the service's role.
	// +optional
	Auth *InstanceAuth `json:"auth,omitempty"`
}

// AuthMethod is how the controller authenticates its admin connection.
// +kubebuilder:validation:Enum=iam;password
type AuthMethod string

const (
	// AuthIAM mints a short-lived RDS IAM token from the ambient AWS
	// credentials. This is the production path and the default.
	AuthIAM AuthMethod = "iam"

	// AuthPassword reads a password from a Secret, for a PostgreSQL without
	// IAM authentication.
	AuthPassword AuthMethod = "password"
)

// InstanceAuth selects how the admin connection authenticates.
//
// +kubebuilder:validation:XValidation:rule="!has(self.method) || self.method != 'password' || has(self.passwordSecretRef)",message="instance.auth.passwordSecretRef is required when auth.method is password"
type InstanceAuth struct {
	// Method is iam or password.
	// +kubebuilder:default=iam
	// +optional
	Method AuthMethod `json:"method,omitempty"`

	// PasswordSecretRef points at the Secret holding the admin password, in
	// this resource's namespace. Required when Method is password.
	// +optional
	PasswordSecretRef *SecretKeySelector `json:"passwordSecretRef,omitempty"`
}

// SecretKeySelector names one key in one Secret.
type SecretKeySelector struct {
	// Name of the Secret, in this resource's namespace.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key within the Secret.
	// +kubebuilder:default=password
	// +optional
	Key string `json:"key,omitempty"`
}

// Engine names the database engine a DatabaseAccess targets. The enum lists
// only what is implemented.
// +kubebuilder:validation:Enum=postgres
type Engine string

const (
	// EnginePostgres covers RDS, Aurora and self-managed PostgreSQL.
	EnginePostgres Engine = "postgres"
)

// Mode selects whether the controller changes the database or only reports
// what it would change.
// +kubebuilder:validation:Enum=Observe;Enforce
type Mode string

const (
	// ModeEnforce applies the plan on every reconcile.
	ModeEnforce Mode = "Enforce"

	// ModeObserve builds the same plan and records it in status without
	// executing it, so a cutover from another provisioner can confirm the plan
	// is empty before the controller is allowed to write.
	ModeObserve Mode = "Observe"
)

// DatabaseAccessSpec declares the desired database state for one service.
type DatabaseAccessSpec struct {
	// Engine is the database engine to provision against.
	// +kubebuilder:default=postgres
	// +optional
	Engine Engine `json:"engine,omitempty"`

	// Instance is the PostgreSQL instance to provision on.
	Instance InstanceRef `json:"instance"`

	// Role is the per-service PostgreSQL role to create and grant to. Single
	// hyphens between other characters are allowed; such a name must be
	// double-quoted in hand-written SQL.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*(-[a-z0-9_]+)*$`
	Role string `json:"role"`

	// GrantRdsIam grants rds_iam to Role so the service can authenticate with
	// an IAM token. Independent of instance.auth.method, which is how the
	// controller authenticates. rds_iam exists only on RDS and Aurora; set this
	// false for a self-managed PostgreSQL.
	// +kubebuilder:default=true
	// +optional
	GrantRdsIam bool `json:"grantRdsIam,omitempty"`

	// Grants are the per-schema privileges.
	// +optional
	Grants []SchemaGrant `json:"grants,omitempty"`

	// RevokeOnDelete revokes the grants when this resource is deleted. The role
	// itself is never dropped. Revocation does not consult other resources: if
	// another DatabaseAccess declares the same role on the same database, it
	// loses the database grants, and any schema both name, until it next
	// reconciles. The controller raises an OverlappingAccess warning event for
	// that case; use one DatabaseAccess per role per database.
	// +kubebuilder:default=false
	// +optional
	RevokeOnDelete bool `json:"revokeOnDelete,omitempty"`

	// Mode is Enforce (apply the plan) or Observe (report in status what would
	// change, and change nothing). Observe also never revokes on delete, even
	// with revokeOnDelete set.
	// +kubebuilder:default=Enforce
	// +optional
	Mode Mode `json:"mode,omitempty"`
}

// DatabaseAccessStatus reports what the controller actually applied.
type DatabaseAccessStatus struct {
	// Conditions are Ready (the last reconcile planned, and in Enforce applied,
	// without a fatal error) and Converged (the database already matches the
	// spec).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the .metadata.generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// AppliedPlanHash fingerprints the statements applied on the last
	// successful reconcile.
	// +optional
	AppliedPlanHash string `json:"appliedPlanHash,omitempty"`

	// LastAppliedTime is when that plan was applied.
	// +optional
	LastAppliedTime *metav1.Time `json:"lastAppliedTime,omitempty"`

	// StatementsApplied counts the statements in the last applied plan.
	// +optional
	StatementsApplied int `json:"statementsApplied,omitempty"`

	// Warnings holds the best-effort statements that failed on the last apply,
	// typically default privileges that keep future objects reachable.
	// +optional
	Warnings []string `json:"warnings,omitempty"`

	// PendingStatements counts the statements the database still needs to
	// match the spec. In Enforce it comes from a re-plan after applying, so
	// non-zero means a statement keeps failing or being undone.
	// +optional
	PendingStatements int `json:"pendingStatements"`

	// Pending lists those statements, capped at the first 50.
	// +optional
	Pending []string `json:"pending,omitempty"`

	// LastPlannedTime is when the database was last planned against, in either
	// mode.
	// +optional
	LastPlannedTime *metav1.Time `json:"lastPlannedTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=dba
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.spec.instance.database`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Pending",type=integer,JSONPath=`.status.pendingStatements`
// +kubebuilder:printcolumn:name="Applied",type=date,JSONPath=`.status.lastAppliedTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DatabaseAccess is the PostgreSQL role, schemas and grants one service needs.
type DatabaseAccess struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DatabaseAccessSpec   `json:"spec,omitempty"`
	Status DatabaseAccessStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DatabaseAccessList is a list of DatabaseAccess resources.
type DatabaseAccessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DatabaseAccess `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DatabaseAccess{}, &DatabaseAccessList{})
}
