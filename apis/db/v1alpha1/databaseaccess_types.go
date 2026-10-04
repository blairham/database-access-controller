// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SchemaGrant declares the privileges a role holds on one schema.
type SchemaGrant struct {
	// Schema is the PostgreSQL schema the privileges apply to. It is created if
	// it does not exist and OwnerOf is true.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	Schema string `json:"schema"`

	// Privileges granted on the schema and on the relations within it.
	//
	// USAGE and CREATE apply to the schema itself. The remainder are granted ON
	// ALL TABLES, and the subset PostgreSQL accepts for sequences (SELECT,
	// UPDATE, USAGE) is granted ON ALL SEQUENCES. Passing INSERT here and
	// letting it reach the sequence grant is an error -- "invalid privilege type
	// INSERT for sequence" -- so the split is done for you.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Enum=SELECT;INSERT;UPDATE;DELETE;TRUNCATE;REFERENCES;TRIGGER;USAGE;CREATE
	Privileges []string `json:"privileges"`

	// OwnerOf makes the role the owner of the schema and of every relation in
	// it: tables, sequences, views and materialized views are reassigned.
	//
	// Views matter and are easy to miss. Replacing a view requires ownership of
	// it, so a migration doing DROP VIEW / CREATE OR REPLACE VIEW fails with
	// "must be owner of view" even when every table it reads was reassigned
	// correctly.
	// +optional
	OwnerOf bool `json:"ownerOf,omitempty"`
}

// InstanceRef identifies the PostgreSQL instance to act on.
//
// The instance may be RDS or Aurora, or a PostgreSQL running anywhere else --
// in this cluster, on a VM, in a development rig. The work the controller does
// once connected is identical in every case, because it is spoken in the
// engine's own protocol; the only thing that varies is how the admin
// connection authenticates. Region is therefore the one AWS-shaped field here,
// and it is required only on the IAM path that uses it.
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

	// Database is the database to connect to and grant CONNECT on.
	// +kubebuilder:default=appdb
	// +optional
	Database string `json:"database,omitempty"`

	// Region is the AWS region, used to sign the IAM auth token. It is
	// required when the admin connection authenticates with IAM -- which is
	// the default -- and ignored entirely under password auth, where there is
	// no token to sign and the instance need not be in AWS at all.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Region string `json:"region,omitempty"`

	// AdminUser is the PostgreSQL role the controller connects as. It needs
	// CREATEROLE and enough membership to reassign ownership.
	// +kubebuilder:default=db_provisioner
	// +optional
	AdminUser string `json:"adminUser,omitempty"`

	// SSLMode is the libpq sslmode for the admin connection. The default,
	// verify-full, checks the server's certificate against the system CAs
	// plus Amazon's RDS CAs (built in) and checks that it names the endpoint.
	// Use verify-ca when the endpoint is a CNAME the certificate does not
	// name. require encrypts without verifying -- the admin credential then
	// goes to whoever answers on the address -- and disable is for a local
	// PostgreSQL without TLS.
	// +kubebuilder:validation:Enum=disable;require;verify-ca;verify-full
	// +kubebuilder:default=verify-full
	// +optional
	SSLMode string `json:"sslMode,omitempty"`

	// Auth is how THE CONTROLLER authenticates as AdminUser.
	//
	// ⚠ Not to be confused with `spec.grantRdsIam`, which grants the rds_iam
	// role to the service's own role. This field governs one connection: the
	// controller's.
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

	// AuthPassword reads a password from a Secret.
	//
	// It exists because IAM authentication is only available on RDS and
	// Aurora. Without it the controller cannot reach a PostgreSQL that is not
	// an AWS managed instance -- a self-managed server, or the one in a local
	// development rig -- which also makes dbctl unusable anywhere but against
	// real RDS.
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

	// PasswordSecretRef points at the Secret holding the admin password. It is
	// required when Method is password and ignored otherwise. The Secret must
	// live in the same namespace as this resource -- a controller that could
	// read Secrets from any namespace on the say-so of a resource in one would
	// be a privilege-escalation path.
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

// Engine names the database engine a DatabaseAccess targets.
//
// The enum lists only what is implemented. Widening it later is additive and
// non-breaking; accepting a value the controller cannot serve would turn a
// typo into a reconcile-time failure instead of an immediate rejection by the
// API server.
// +kubebuilder:validation:Enum=postgres
type Engine string

const (
	// EnginePostgres covers RDS PostgreSQL and Aurora PostgreSQL. They speak
	// the same wire protocol and take the same DDL, so one implementation
	// serves both.
	EnginePostgres Engine = "postgres"
)

// Mode selects whether the controller changes the database or only reports
// what it would change.
// +kubebuilder:validation:Enum=Observe;Enforce
type Mode string

const (
	// ModeEnforce applies the plan: the database is brought to the declared
	// state on every reconcile.
	ModeEnforce Mode = "Enforce"

	// ModeObserve builds the same plan and records it in status without
	// executing a single statement.
	//
	// It exists for cutovers. Turning a DatabaseAccess on next to whatever
	// provisioned the database before -- a Job, a runbook, a human -- in
	// Enforce proves only that the controller did no harm, because the old
	// provisioner's grants are already there to hide a controller that does
	// nothing. Observe answers the question that matters first: against the
	// real database, what exactly would this controller change? An empty
	// plan on a database the old provisioner converged is the evidence that
	// the two agree, gathered before the controller is allowed to write.
	ModeObserve Mode = "Observe"
)

// DatabaseAccessSpec declares the desired database state for one service.
type DatabaseAccessSpec struct {
	// Engine is the database engine to provision against.
	//
	// The field is explicit even while only one value is valid, so adding an
	// engine is an enum widening rather than a breaking change to a spec that
	// had assumed PostgreSQL all along.
	// +kubebuilder:default=postgres
	// +optional
	Engine Engine `json:"engine,omitempty"`

	// Instance is the RDS instance to provision on.
	Instance InstanceRef `json:"instance"`

	// Role is the per-service PostgreSQL role to create and grant to.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	Role string `json:"role"`

	// GrantRdsIam grants the rds_iam role to Role, so the SERVICE can
	// authenticate to the database with an IAM token instead of a password.
	//
	// ⚠ THIS IS NOT HOW THE CONTROLLER AUTHENTICATES. That is
	// `instance.auth.method`, and the two are independent: the controller can
	// connect with a password to a real RDS instance and still grant rds_iam,
	// or connect with an IAM token and grant nothing.
	//
	// The rds_iam role exists only on RDS and Aurora. Set this false against a
	// self-managed PostgreSQL, or the grant fails.
	// +kubebuilder:default=true
	// +optional
	GrantRdsIam bool `json:"grantRdsIam,omitempty"`

	// Grants are the per-schema privileges.
	// +optional
	Grants []SchemaGrant `json:"grants,omitempty"`

	// RevokeOnDelete drops the grants when this resource is deleted.
	//
	// Defaults to false, and that default is deliberate: role-owned objects
	// break on DROP, so tearing down access is a decision a human makes with
	// the data in front of them.
	// +kubebuilder:default=false
	// +optional
	RevokeOnDelete bool `json:"revokeOnDelete,omitempty"`

	// Mode is Enforce (apply the plan) or Observe (plan only: report in
	// status what would change, and change nothing).
	//
	// Observe never executes a statement, never adds the revoke finalizer and
	// never revokes on delete, even with revokeOnDelete set. Switching a
	// resource from Observe to Enforce is the cutover; switching back stops
	// all writes on the next reconcile.
	// +kubebuilder:default=Enforce
	// +optional
	Mode Mode `json:"mode,omitempty"`
}

// DatabaseAccessStatus reports what the controller actually applied.
type DatabaseAccessStatus struct {
	// Conditions follow the standard Kubernetes condition contract. Ready is
	// true when the last reconcile applied every statement without a fatal
	// error.
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

	// Warnings holds the best-effort statements that failed. These are not
	// fatal, but they are the difference between "access was granted" and
	// "access was granted and will survive the next migration", so they are
	// surfaced rather than buried in a pod log.
	// +optional
	Warnings []string `json:"warnings,omitempty"`

	// PendingStatements counts the statements the database still needs to
	// match the spec, from the plan built on the last reconcile.
	//
	// In Observe it is the size of the plan that was not applied. In Enforce
	// it comes from a fresh plan built after applying, so it is what the
	// database STILL lacks -- non-zero there means a statement keeps failing
	// or keeps being undone, not that the controller is behind.
	// +optional
	PendingStatements int `json:"pendingStatements"`

	// Pending lists those statements, capped at the first 50. The list is
	// truncated whenever it is shorter than pendingStatements.
	// +optional
	Pending []string `json:"pending,omitempty"`

	// LastPlannedTime is when the database was last read and planned against,
	// in either mode. LastAppliedTime does not move in Observe; this does.
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
