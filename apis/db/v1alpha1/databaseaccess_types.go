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

// InstanceRef identifies the RDS instance to act on.
type InstanceRef struct {
	// Endpoint is the RDS endpoint hostname.
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

	// Region is the AWS region, used to sign the IAM auth token.
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`

	// AdminUser is the PostgreSQL role the controller connects as. It needs
	// CREATEROLE and enough membership to reassign ownership.
	// +kubebuilder:default=db_provisioner
	// +optional
	AdminUser string `json:"adminUser,omitempty"`

	// SSLMode is the libpq sslmode for the admin connection.
	// +kubebuilder:validation:Enum=disable;require;verify-ca;verify-full
	// +kubebuilder:default=require
	// +optional
	SSLMode string `json:"sslMode,omitempty"`

	// Auth is how the controller authenticates as AdminUser.
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

	// IAMAuth grants rds_iam to the role, so the service authenticates with an
	// IAM token rather than a password.
	// +kubebuilder:default=true
	// +optional
	IAMAuth bool `json:"iamAuth,omitempty"`

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
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=dba
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.spec.instance.database`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
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
