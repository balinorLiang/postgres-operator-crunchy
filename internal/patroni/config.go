// Copyright 2021 - 2025 Crunchy Data Solutions, Inc.
//
// SPDX-License-Identifier: Apache-2.0

package patroni

import (
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/crunchydata/postgres-operator/internal/config"
	"github.com/crunchydata/postgres-operator/internal/naming"
	"github.com/crunchydata/postgres-operator/internal/postgres"
	"github.com/crunchydata/postgres-operator/pkg/apis/postgres-operator.crunchydata.com/v1beta1"
)

const (
	configDirectory  = "/etc/patroni"
	configMapFileKey = "patroni.yaml"
)

const (
	basebackupCreateReplicaMethod = "basebackup"
	pgBackRestCreateReplicaMethod = "pgbackrest"
)

const (
	yamlGeneratedWarning = "" +
		"# Generated by postgres-operator. DO NOT EDIT.\n" +
		"# Your changes will not be saved.\n"
)

// quoteShellWord ensures that s is interpreted by a shell as single word.
func quoteShellWord(s string) string {
	// https://www.gnu.org/software/bash/manual/html_node/Quoting.html
	return `'` + strings.ReplaceAll(s, `'`, `'"'"'`) + `'`
}

// clusterYAML returns Patroni settings that apply to the entire cluster.
func clusterYAML(
	cluster *v1beta1.PostgresCluster,
	pgHBAs postgres.HBAs, pgParameters postgres.Parameters, patroniLogStorageLimit int64,
) (string, error) {
	root := map[string]any{
		// The cluster identifier. This value cannot change during the cluster's
		// lifetime.
		"scope": naming.PatroniScope(cluster),

		// Use Kubernetes Endpoints for the distributed configuration store (DCS).
		// These values cannot change during the cluster's lifetime.
		//
		// NOTE(cbandy): It *might* be possible to *carefully* change the role and
		// scope labels, but there is no way to reconfigure all instances at once.
		"kubernetes": map[string]any{
			"namespace":     cluster.Namespace,
			"role_label":    naming.LabelRole,
			"scope_label":   naming.LabelPatroni,
			"use_endpoints": true,
			// To support transitioning to Patroni v4, set the value to 'master'.
			// In a future release, this can be removed in favor of the default.
			"leader_label_value": naming.RolePatroniLeader,

			// In addition to "scope_label" above, Patroni will add the following to
			// every object it creates. It will also use these as filters when doing
			// any lookups.
			"labels": map[string]string{
				naming.LabelCluster: cluster.Name,
			},
		},

		"postgresql": map[string]any{
			// TODO(cbandy): "callbacks"

			// Custom configuration "must exist on all cluster nodes".
			//
			// TODO(cbandy): I imagine we will always set this to a file we own. At
			// the very least, it will start with an "include_dir" directive.
			// - https://www.postgresql.org/docs/current/config-setting.html#CONFIG-INCLUDES
			//"custom_conf": nil,

			// TODO(cbandy): Should "parameters", "pg_hba", and "pg_ident" be set in
			// DCS? If so, are they are automatically regenerated and reloaded?

			// PostgreSQL Auth settings used by Patroni to
			// create replication, and pg_rewind accounts
			// TODO(tjmoore4): add "superuser" account
			"authentication": map[string]any{
				"replication": map[string]any{
					"sslcert":     "/tmp/replication/tls.crt",
					"sslkey":      "/tmp/replication/tls.key",
					"sslmode":     "verify-ca",
					"sslrootcert": "/tmp/replication/ca.crt",
					"username":    postgres.ReplicationUser,
				},
				"rewind": map[string]any{
					"sslcert":     "/tmp/replication/tls.crt",
					"sslkey":      "/tmp/replication/tls.key",
					"sslmode":     "verify-ca",
					"sslrootcert": "/tmp/replication/ca.crt",
					"username":    postgres.ReplicationUser,
				},
			},
		},

		// NOTE(cbandy): Every Patroni instance is a client of every other Patroni
		// instance. TLS and/or authentication settings need to be applied consistently
		// across the entire cluster.

		"restapi": map[string]any{
			// Use TLS to encrypt traffic and verify clients.
			// NOTE(cbandy): The path package always uses slash separators.
			"cafile":   path.Join(configDirectory, certAuthorityConfigPath),
			"certfile": path.Join(configDirectory, certServerConfigPath),

			// The private key is bundled into "restapi.certfile".
			"keyfile": nil,

			// Require clients to present a certificate verified by "restapi.cafile"
			// when calling "unsafe" API endpoints.
			// - https://github.com/zalando/patroni/blob/v2.0.1/docs/security.rst#protecting-the-rest-api
			//
			// NOTE(cbandy): We'd prefer "required" here, but Kubernetes HTTPS probes
			// offer no way to present client certificates. Perhaps Patroni could change
			// to relax the requirement on *just* liveness and readiness?
			// - https://issue.k8s.io/92647
			"verify_client": "optional",

			// TODO(cbandy): The next release of Patroni will allow more control over
			// the TLS protocols/ciphers.
			// Maybe "ciphers": "EECDH+AESGCM+FIPS:EDH+AESGCM+FIPS". Maybe add ":!DHE".
			// - https://github.com/zalando/patroni/commit/ba4ab58d4069ee30
		},

		"ctl": map[string]any{
			// Use TLS to verify the server and present a client certificate.
			// NOTE(cbandy): The path package always uses slash separators.
			"cacert":   path.Join(configDirectory, certAuthorityConfigPath),
			"certfile": path.Join(configDirectory, certServerConfigPath),

			// The private key is bundled into "ctl.certfile".
			"keyfile": nil,

			// Always verify the server certificate against "ctl.cacert".
			"insecure": false,
		},

		"watchdog": map[string]any{
			// Disable leader watchdog device. Kubernetes' liveness probe is a less
			// flexible approximation.
			"mode": "off",
		},
	}

	// if a Patroni log file size is configured, configure volume file storage
	if patroniLogStorageLimit != 0 {

		// Configure the Patroni log settings
		// - https://patroni.readthedocs.io/en/latest/yaml_configuration.html#log
		root["log"] = map[string]any{

			"dir":  naming.PatroniPGDataLogPath,
			"type": "json",

			// defaults to "INFO"
			"level": cluster.Spec.Patroni.Logging.Level,

			// There will only be two log files. Cannot set to 1 or the logs won't rotate.
			// - https://github.com/python/cpython/blob/3.11/Lib/logging/handlers.py#L134
			"file_num": 1,

			// Since there are two log files, ensure the total space used is under
			// the configured limit.
			"file_size": patroniLogStorageLimit / 2,
		}
	}

	if !ClusterBootstrapped(cluster) {
		// Patroni has not yet bootstrapped. Populate the "bootstrap.dcs" field to
		// facilitate it. When Patroni is already bootstrapped, this field is ignored.

		root["bootstrap"] = map[string]any{
			"dcs": DynamicConfiguration(&cluster.Spec, pgHBAs, pgParameters),

			// Missing here is "users" which runs *after* "post_bootstrap". It is
			// not possible to use roles created by the former in the latter.
			// - https://github.com/zalando/patroni/issues/667
		}
	}

	b, err := yaml.Marshal(root)
	return string(append([]byte(yamlGeneratedWarning), b...)), err
}

// DynamicConfiguration combines configuration with some PostgreSQL settings
// and returns a value that can be marshaled to JSON.
func DynamicConfiguration(
	spec *v1beta1.PostgresClusterSpec,
	pgHBAs postgres.HBAs, pgParameters postgres.Parameters,
) map[string]any {
	// Copy the entire configuration before making any changes.
	root := make(map[string]any)
	if spec.Patroni != nil && spec.Patroni.DynamicConfiguration != nil {
		root = spec.Patroni.DynamicConfiguration.DeepCopy()
	}

	// NOTE: These are always populated due to [v1beta1.PatroniSpec.Default]
	root["ttl"] = *spec.Patroni.LeaderLeaseDurationSeconds
	root["loop_wait"] = *spec.Patroni.SyncPeriodSeconds

	postgresql := map[string]any{
		// TODO(cbandy): explain this. requires an archive, perhaps.
		"use_slots": false,
	}

	// When TDE is configured, override the pg_rewind binary name to point
	// to the wrapper script.
	if config.FetchKeyCommand(spec) != "" {
		postgresql["bin_name"] = map[string]any{
			"pg_rewind": "/tmp/pg_rewind_tde.sh",
		}
	}

	// Copy the "postgresql" section over the above defaults.
	if section, ok := root["postgresql"].(map[string]any); ok {
		for k, v := range section {
			postgresql[k] = v
		}
	}
	root["postgresql"] = postgresql

	// Copy the "postgresql.parameters" section over any defaults.
	parameters := make(map[string]any)
	if pgParameters.Default != nil {
		for k, v := range pgParameters.Default.AsMap() {
			parameters[k] = v
		}
	}
	if section, ok := postgresql["parameters"].(map[string]any); ok {
		for k, v := range section {
			parameters[k] = v
		}
	}
	// Override the above with mandatory parameters.
	if pgParameters.Mandatory != nil {
		for k, v := range pgParameters.Mandatory.AsMap() {

			// This parameter is a comma-separated list. Rather than overwrite the
			// user-defined value, we want to combine it with the mandatory one.
			// Some libraries belong at specific positions in the list, so figure
			// that out as well.
			if k == "shared_preload_libraries" {
				// Load mandatory libraries ahead of user-defined libraries.
				if s, ok := parameters[k].(string); ok && len(s) > 0 {
					v = v + "," + s
				}
				// Load "citus" ahead of any other libraries.
				// - https://github.com/citusdata/citus/blob/v12.0.0/src/backend/distributed/shared_library_init.c#L417-L419
				if strings.Contains(v, "citus") {
					v = "citus," + v
				}
			}

			parameters[k] = v
		}
	}
	postgresql["parameters"] = parameters
	postgresql["database"] = "highgo"

	// Copy the "postgresql.pg_hba" section after any mandatory values.
	hba := make([]string, 0, len(pgHBAs.Mandatory))
	for i := range pgHBAs.Mandatory {
		hba = append(hba, pgHBAs.Mandatory[i].String())
	}
	if section, ok := postgresql["pg_hba"].([]any); ok {
		for i := range section {
			// any pg_hba values that are not strings will be skipped
			if value, ok := section[i].(string); ok {
				hba = append(hba, value)
			}
		}
	}
	// When the section is missing or empty, include the recommended defaults.
	if len(hba) == len(pgHBAs.Mandatory) {
		for i := range pgHBAs.Default {
			hba = append(hba, pgHBAs.Default[i].String())
		}
	}
	postgresql["pg_hba"] = hba

	// Enabling `pg_rewind` allows a former primary to automatically rejoin the
	// cluster even if it has commits that were not sent to a replica. In other
	// words, this favors availability over consistency. Without it, the former
	// primary needs patronictl reinit to rejoin.
	//
	// Recent versions of `pg_rewind` can run with limited permissions granted
	// by Patroni to the user defined in "postgresql.authentication.rewind".
	// PostgreSQL v10 and earlier require superuser access over the network.
	postgresql["use_pg_rewind"] = spec.PostgresVersion > 10

	if spec.Standby != nil && spec.Standby.Enabled {
		standby, _ := root["standby_cluster"].(map[string]any)
		if standby == nil {
			standby = make(map[string]any)
		}

		// Unset any previous value for restore_command - we will set it later if needed
		delete(standby, "restore_command")

		// Populate replica creation methods based on options provided in the standby spec:
		methods := []string{}
		if spec.Standby.Host != "" {
			standby["host"] = spec.Standby.Host
			if spec.Standby.Port != nil {
				standby["port"] = *spec.Standby.Port
			}

			methods = append([]string{basebackupCreateReplicaMethod}, methods...)
		}

		if spec.Standby.RepoName != "" {
			// Append pgbackrest as the first choice when creating the standby
			methods = append([]string{pgBackRestCreateReplicaMethod}, methods...)

			// Populate the standby leader by shipping logs through pgBackRest.
			// This also overrides the "restore_command" used by standby replicas.
			// - https://www.postgresql.org/docs/current/warm-standby.html
			standby["restore_command"] = pgParameters.Mandatory.Value("restore_command")
		}

		standby["create_replica_methods"] = methods
		root["standby_cluster"] = standby
	}

	return root
}

// instanceEnvironment returns the environment variables needed by Patroni's
// instance container.
func instanceEnvironment(
	cluster *v1beta1.PostgresCluster,
	clusterPodService *corev1.Service,
	leaderService *corev1.Service,
	podContainers []corev1.Container,
) []corev1.EnvVar {
	var (
		patroniPort  = *cluster.Spec.Patroni.Port
		postgresPort = *cluster.Spec.Port
		podSubdomain = clusterPodService.Name
	)

	// Gather Endpoint ports for any Container ports that match the leader
	// Service definition.
	ports := []corev1.EndpointPort{}
	for _, sp := range leaderService.Spec.Ports {
		for i := range podContainers {
			for _, cp := range podContainers[i].Ports {
				if sp.TargetPort.StrVal == cp.Name {
					ports = append(ports, corev1.EndpointPort{
						Name:     sp.Name,
						Port:     cp.ContainerPort,
						Protocol: cp.Protocol,
					})
				}
			}
		}
	}
	portsYAML, _ := yaml.Marshal(ports)

	// NOTE(cbandy): Patroni consumes and then removes environment variables
	// starting with "PATRONI_".
	// - https://github.com/zalando/patroni/blob/v2.0.2/patroni/config.py#L247
	// - https://github.com/zalando/patroni/blob/v2.0.2/patroni/postgresql/postmaster.py#L215-L216

	variables := []corev1.EnvVar{
		// Set "name" to the v1.Pod's name. Required when using Kubernetes for DCS.
		// Patroni must be restarted when changing this value.
		{
			Name: "PATRONI_NAME",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
				APIVersion: "v1",
				FieldPath:  "metadata.name",
			}},
		},

		// Set "kubernetes.pod_ip" to the v1.Pod's primary IP address.
		// Patroni must be restarted when changing this value.
		{
			Name: "PATRONI_KUBERNETES_POD_IP",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
				APIVersion: "v1",
				FieldPath:  "status.podIP",
			}},
		},

		// When using Endpoints for DCS, Patroni needs to replicate the leader
		// ServicePort definitions. Set "kubernetes.ports" to the YAML of this
		// Pod's equivalent EndpointPort definitions.
		//
		// This is connascent with PATRONI_POSTGRESQL_CONNECT_ADDRESS below.
		// Patroni must be restarted when changing this value.
		{
			Name:  "PATRONI_KUBERNETES_PORTS",
			Value: string(portsYAML),
		},

		// Set "postgresql.connect_address" using the Pod's stable DNS name.
		// PostgreSQL must be restarted when changing this value.
		{
			Name:  "PATRONI_POSTGRESQL_CONNECT_ADDRESS",
			Value: fmt.Sprintf("%s.%s:%d", "$(PATRONI_NAME)", podSubdomain, postgresPort),
		},

		// Set "postgresql.listen" using the special address "*" to mean all TCP
		// interfaces. When connecting locally over TCP, Patroni will use "localhost".
		//
		// This is connascent with PATRONI_POSTGRESQL_CONNECT_ADDRESS above.
		// PostgreSQL must be restarted when changing this value.
		{
			Name:  "PATRONI_POSTGRESQL_LISTEN",
			Value: fmt.Sprintf("*:%d", postgresPort),
		},

		// Set "postgresql.config_dir" to PostgreSQL's $PGDATA directory.
		// Patroni must be restarted when changing this value.
		{
			Name:  "PATRONI_POSTGRESQL_CONFIG_DIR",
			Value: postgres.ConfigDirectory(cluster),
		},

		// Set "postgresql.data_dir" to PostgreSQL's "data_directory".
		// Patroni must be restarted when changing this value.
		{
			Name:  "PATRONI_POSTGRESQL_DATA_DIR",
			Value: postgres.DataDirectory(cluster),
		},

		// Set "restapi.connect_address" using the Pod's stable DNS name.
		// Patroni must be reloaded when changing this value.
		{
			Name:  "PATRONI_RESTAPI_CONNECT_ADDRESS",
			Value: fmt.Sprintf("%s.%s:%d", "$(PATRONI_NAME)", podSubdomain, patroniPort),
		},

		// Set "restapi.listen" using the special address "*" to mean all TCP interfaces.
		// This is connascent with PATRONI_RESTAPI_CONNECT_ADDRESS above.
		// Patroni must be reloaded when changing this value.
		{
			Name:  "PATRONI_RESTAPI_LISTEN",
			Value: fmt.Sprintf("*:%d", patroniPort),
		},

		// The Patroni client `patronictl` looks here for its configuration file(s).
		{
			Name:  "PATRONICTL_CONFIG_FILE",
			Value: configDirectory,
		},
	}

	return variables
}

// instanceConfigFiles returns projections of Patroni's configuration files
// to include in the instance configuration volume.
func instanceConfigFiles(cluster, instance *corev1.ConfigMap) []corev1.VolumeProjection {
	return []corev1.VolumeProjection{
		{
			ConfigMap: &corev1.ConfigMapProjection{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: cluster.Name,
				},
				Items: []corev1.KeyToPath{{
					Key:  configMapFileKey,
					Path: "~postgres-operator_cluster.yaml",
				}},
			},
		},
		{
			ConfigMap: &corev1.ConfigMapProjection{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: instance.Name,
				},
				Items: []corev1.KeyToPath{{
					Key:  configMapFileKey,
					Path: "~postgres-operator_instance.yaml",
				}},
			},
		},
	}
}

// instanceYAML returns Patroni settings that apply to instance.
func instanceYAML(
	cluster *v1beta1.PostgresCluster, instance *v1beta1.PostgresInstanceSetSpec,
	pgbackrestReplicaCreateCommand []string,
) (string, error) {
	root := map[string]any{
		// Missing here is "name" which cannot be known until the instance Pod is
		// created. That value should be injected using the downward API and the
		// PATRONI_NAME environment variable.

		"kubernetes": map[string]any{
			// Missing here is "pod_ip" which cannot be known until the instance Pod is
			// created. That value should be injected using the downward API and the
			// PATRONI_KUBERNETES_POD_IP environment variable.

			// Missing here is "ports" which is is connascent with "postgresql.connect_address".
			// See the PATRONI_KUBERNETES_PORTS env variable.
		},

		"restapi": map[string]any{
			// Missing here is "connect_address" which cannot be known until the
			// instance Pod is created. That value should be injected using the downward
			// API and the PATRONI_RESTAPI_CONNECT_ADDRESS environment variable.

			// Missing here is "listen" which is connascent with "connect_address".
			// See the PATRONI_RESTAPI_LISTEN environment variable.
		},

		"tags": map[string]any{
			// TODO(cbandy): "nofailover"
			// TODO(cbandy): "nosync"
		},
	}

	postgresql := map[string]any{
		// TODO(cbandy): "bin_dir"

		// Missing here is "connect_address" which cannot be known until the
		// instance Pod is created. That value should be injected using the downward
		// API and the PATRONI_POSTGRESQL_CONNECT_ADDRESS environment variable.

		// Missing here is "listen" which is connascent with "connect_address".
		// See the PATRONI_POSTGRESQL_LISTEN environment variable.

		// During startup, Patroni checks that this path is writable whether we use passwords or not.
		// - https://github.com/zalando/patroni/issues/1888
		"pgpass": "/tmp/.pgpass",

		// Prefer to use UNIX domain sockets for local connections. If the PostgreSQL
		// parameter "unix_socket_directories" is set, Patroni will connect using one
		// of those directories. Otherwise, it will use the client (libpq) default.
		"use_unix_socket": true,
	}
	root["postgresql"] = postgresql

	// The "basebackup" replica method is configured differently from others.
	// Patroni prepends "--" before it calls `pg_basebackup`.
	// - https://github.com/zalando/patroni/blob/v2.0.2/patroni/postgresql/bootstrap.py#L45
	postgresql["basebackup"] = []string{
		// NOTE(cbandy): The "--waldir" option was introduced in PostgreSQL v10.
		"waldir=" + postgres.WALDirectory(cluster, instance),
	}
	methods := []string{"basebackup"}

	// Prefer a pgBackRest method when it is available, and fallback to other
	// methods when it fails.
	if command := pgbackrestReplicaCreateCommand; len(command) > 0 {

		// Regardless of the "keep_data" setting below, Patroni deletes the
		// data directory when all methods fail. pgBackRest will not restore
		// when the data directory is missing, so create it before running the
		// command. PostgreSQL requires that the directory is writable by only
		// itself.
		// - https://github.com/zalando/patroni/blob/v2.0.2/patroni/ha.py#L249
		// - https://github.com/pgbackrest/pgbackrest/issues/1445
		// - https://git.postgresql.org/gitweb/?p=postgresql.git;f=src/backend/utils/init/miscinit.c;hb=REL_13_0#l319
		//
		// NOTE(cbandy): The "PATRONI_POSTGRESQL_DATA_DIR" environment variable
		// is defined in this package, but it is removed by Patroni at runtime.
		command = append([]string{
			"bash", "-ceu", "--",
			`install --directory --mode=0700 "${PGDATA?}" && exec "$@"`,
			"-",
		}, command...)

		quoted := make([]string, len(command))
		for i := range command {
			quoted[i] = quoteShellWord(command[i])
		}
		postgresql[pgBackRestCreateReplicaMethod] = map[string]any{
			"command":   strings.Join(quoted, " "),
			"keep_data": true,
			"no_leader": true,
			"no_params": true,
		}
		methods = append([]string{pgBackRestCreateReplicaMethod}, methods...)
	}

	// NOTE(cbandy): Is there any chance a user might want to specify their own
	// method? This is a list and cannot be merged.
	postgresql["create_replica_methods"] = methods

	if !ClusterBootstrapped(cluster) {
		isRestore := (cluster.Status.PGBackRest != nil && cluster.Status.PGBackRest.Restore != nil)
		isDataSource := (cluster.Spec.DataSource != nil && cluster.Spec.DataSource.Volumes != nil &&
			cluster.Spec.DataSource.Volumes.PGDataVolume != nil &&
			cluster.Spec.DataSource.Volumes.PGDataVolume.Directory != "")
		// If the cluster is being bootstrapped using existing volumes, or if the cluster is being
		// bootstrapped following a restore, then use the "existing"
		// bootstrap method.  Otherwise use "initdb".
		if isRestore || isDataSource {
			data_dir := postgres.DataDirectory(cluster)
			root["bootstrap"] = map[string]any{
				"method": "existing",
				"existing": map[string]any{
					"command":   fmt.Sprintf(`mv %q %q`, data_dir+"_bootstrap", data_dir),
					"no_params": "true",
				},
			}
		} else {
			encrption := "scram-sha-256"
			if cluster.Spec.Patroni != nil {
				if postgresql, ok := cluster.Spec.Patroni.DynamicConfiguration["postgresql"]; ok {
					root := postgresql.(map[string]interface{})
					if pwdEncrypt, ok := root["password_encryption"]; ok {
						if encrypt, ok := pwdEncrypt.(string); ok {
							encrption = encrypt
						}
					}
				}
			}

			initdb := []string{
				// Enable checksums on data pages to help detect corruption of
				// storage that would otherwise be silent. This also enables
				// "wal_log_hints" which is a prerequisite for using `pg_rewind`.
				// - https://www.postgresql.org/docs/current/app-initdb.html
				// - https://www.postgresql.org/docs/current/app-pgrewind.html
				// - https://www.postgresql.org/docs/current/runtime-config-wal.html
				//
				// The benefits of checksums in the Kubernetes storage landscape
				// outweigh their negligible overhead, and enabling them later
				// is costly. (Every file of the cluster must be rewritten.)
				// PostgreSQL v12 introduced the `pg_checksums` utility which
				// can cheaply disable them while PostgreSQL is stopped.
				// - https://www.postgresql.org/docs/current/app-pgchecksums.html
				"data-checksums",
				"encoding=UTF8",
				"auth=" + encrption,
				// NOTE(cbandy): The "--waldir" option was introduced in PostgreSQL v10.
				"waldir=" + postgres.WALDirectory(cluster, instance),
			}

			// Append the encryption key command, if provided.
			if ekc := config.FetchKeyCommand(&cluster.Spec); ekc != "" {
				initdb = append(initdb, fmt.Sprintf("encryption-key-command=%s", ekc))
			}

			// Populate some "bootstrap" fields to initialize the cluster.
			// When Patroni is already bootstrapped, this section is ignored.
			// - https://github.com/zalando/patroni/blob/v2.0.2/docs/SETTINGS.rst#bootstrap-configuration
			// - https://github.com/zalando/patroni/blob/v2.0.2/docs/replica_bootstrap.rst#bootstrap
			root["bootstrap"] = map[string]any{
				"method": "initdb",

				// The "initdb" bootstrap method is configured differently from others.
				// Patroni prepends "--" before it calls `initdb`.
				// - https://github.com/zalando/patroni/blob/v2.0.2/patroni/postgresql/bootstrap.py#L45
				"initdb": initdb,
			}
		}
	}

	b, err := yaml.Marshal(root)
	return string(append([]byte(yamlGeneratedWarning), b...)), err
}

// probeTiming returns a Probe with thresholds and timeouts set according to spec.
func probeTiming(spec *v1beta1.PatroniSpec) *corev1.Probe {
	// "Probes should be configured in such a way that they start failing about
	// time when the leader key is expiring."
	// - https://github.com/zalando/patroni/blob/v2.0.1/docs/rest_api.rst
	// - https://github.com/zalando/patroni/blob/v2.0.1/docs/watchdog.rst

	// TODO(cbandy): When the probe times out, failure triggers at
	// (FailureThreshold × PeriodSeconds + TimeoutSeconds)
	probe := corev1.Probe{
		TimeoutSeconds:   *spec.SyncPeriodSeconds / 2,
		PeriodSeconds:    *spec.SyncPeriodSeconds,
		SuccessThreshold: 1,
		FailureThreshold: *spec.LeaderLeaseDurationSeconds / *spec.SyncPeriodSeconds,
	}

	if probe.TimeoutSeconds < 1 {
		probe.TimeoutSeconds = 1
	}
	if probe.FailureThreshold < 1 {
		probe.FailureThreshold = 1
	}

	return &probe
}
