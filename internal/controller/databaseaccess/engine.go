// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package databaseaccess

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbv1alpha1 "github.com/blairham/database-access-controller/apis/db/v1alpha1"
	"github.com/blairham/database-access-controller/internal/engine"
	"github.com/blairham/database-access-controller/internal/engine/postgres"
	"github.com/blairham/database-access-controller/internal/rdsauth"
)

// EngineFactory opens an engine for one resource. It takes the whole object
// because a password Secret is resolved in the resource's namespace.
type EngineFactory func(ctx context.Context, da *dbv1alpha1.DatabaseAccess) (engine.Engine, error)

// NewEngineFactory returns the production factory. The client only reads a
// password Secret, and only from the resource's own namespace.
func NewEngineFactory(c client.Client) EngineFactory {
	return func(ctx context.Context, da *dbv1alpha1.DatabaseAccess) (engine.Engine, error) {
		spec := da.Spec

		// The CRD enum normally rejects this at admission.
		switch spec.Engine {
		case "", dbv1alpha1.EnginePostgres:
		default:
			return nil, fmt.Errorf("unsupported engine %q: this controller implements %q",
				spec.Engine, dbv1alpha1.EnginePostgres)
		}

		inst := spec.Instance
		connCfg := postgres.ConnConfig{
			Host:     inst.Endpoint,
			Port:     inst.Port,
			Database: database(inst),
			User:     adminUser(inst),
			SSLMode:  inst.SSLMode,
		}

		switch authMethod(inst) {
		case dbv1alpha1.AuthPassword:
			password, err := resolvePassword(ctx, c, da)
			if err != nil {
				return nil, err
			}
			connCfg.Password = password
		default:
			tokens, err := rdsauth.New(ctx, rdsauth.Config{
				Host:   inst.Endpoint,
				Port:   inst.Port,
				Region: inst.Region,
				User:   adminUser(inst),
			})
			if err != nil {
				return nil, fmt.Errorf("preparing IAM auth: %w", err)
			}
			connCfg.Tokens = tokens
		}

		conn, err := postgres.Connect(ctx, connCfg)
		if err != nil {
			return nil, err
		}

		closeFn := func() error { return conn.Close(context.WithoutCancel(ctx)) }
		return postgres.New(conn, conn, closeFn), nil
	}
}

// resolvePassword reads the admin password from the Secret the resource names.
func resolvePassword(ctx context.Context, c client.Client, da *dbv1alpha1.DatabaseAccess) (string, error) {
	auth := da.Spec.Instance.Auth
	if auth == nil || auth.PasswordSecretRef == nil {
		return "", fmt.Errorf("auth.method is %q but auth.passwordSecretRef is unset", dbv1alpha1.AuthPassword)
	}
	ref := auth.PasswordSecretRef

	key := ref.Key
	if key == "" {
		key = "password"
	}

	var secret corev1.Secret
	// Namespace is taken from the resource, never from the reference.
	name := client.ObjectKey{Namespace: da.Namespace, Name: ref.Name}
	if err := c.Get(ctx, name, &secret); err != nil {
		return "", fmt.Errorf("reading secret %s: %w", name, err)
	}

	raw, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("secret %s has no key %q (keys: %s)",
			name, key, strings.Join(secretKeys(secret), ", "))
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("secret %s key %q is empty", name, key)
	}
	return string(raw), nil
}

func secretKeys(s corev1.Secret) []string {
	keys := make([]string, 0, len(s.Data))
	for k := range s.Data {
		keys = append(keys, k)
	}
	return keys
}

func authMethod(i dbv1alpha1.InstanceRef) dbv1alpha1.AuthMethod {
	if i.Auth == nil || i.Auth.Method == "" {
		return dbv1alpha1.AuthIAM
	}
	return i.Auth.Method
}

func adminUser(i dbv1alpha1.InstanceRef) string {
	if i.AdminUser == "" {
		return "db_provisioner"
	}
	return i.AdminUser
}

func database(i dbv1alpha1.InstanceRef) string {
	if i.Database == "" {
		return "appdb"
	}
	return i.Database
}
