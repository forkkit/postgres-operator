// Package cluster holds the cluster CRD logic and definitions
// A cluster is comprised of a primary service, replica service,
// primary deployment, and replica deployment
package cluster

/*
 Copyright 2017-2018 Crunchy Data Solutions, Inc.
 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

      http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

import (
	log "github.com/Sirupsen/logrus"
	crv1 "github.com/crunchydata/postgres-operator/apis/cr/v1"
	"github.com/crunchydata/postgres-operator/kubeapi"
	"github.com/crunchydata/postgres-operator/operator"
	"github.com/crunchydata/postgres-operator/operator/pvc"
	"github.com/crunchydata/postgres-operator/util"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"k8s.io/client-go/rest"
	"strconv"
)

// Strategy ....
type Strategy interface {
	Scale(*kubernetes.Clientset, *rest.RESTClient, *crv1.Pgreplica, string, string, *crv1.Pgcluster) error
	AddCluster(*kubernetes.Clientset, *rest.RESTClient, *crv1.Pgcluster, string, string) error
	Failover(*kubernetes.Clientset, *rest.RESTClient, string, *crv1.Pgtask, string, *rest.Config) error
	CreateReplica(string, *kubernetes.Clientset, *crv1.Pgcluster, string, string, string) error
	DeleteCluster(*kubernetes.Clientset, *rest.RESTClient, *crv1.Pgcluster, string) error
	DeleteReplica(*kubernetes.Clientset, *crv1.Pgreplica, string) error

	MinorUpgrade(*kubernetes.Clientset, *rest.RESTClient, *crv1.Pgcluster, *crv1.Pgupgrade, string) error
	MajorUpgrade(*kubernetes.Clientset, *rest.RESTClient, *crv1.Pgcluster, *crv1.Pgupgrade, string) error
	MajorUpgradeFinalize(*kubernetes.Clientset, *rest.RESTClient, *crv1.Pgcluster, *crv1.Pgupgrade, string) error
	UpdatePolicyLabels(*kubernetes.Clientset, string, string, map[string]string) error
}

// ServiceTemplateFields ...
type ServiceTemplateFields struct {
	Name        string
	ClusterName string
	Port        string
	ServiceType string
}

// DeploymentTemplateFields ...
type DeploymentTemplateFields struct {
	Name               string
	ClusterName        string
	Port               string
	PgMode             string
	CCPImagePrefix     string
	CCPImageTag        string
	Database           string
	OperatorLabels     string
	DataPathOverride   string
	ArchiveMode        string
	ArchivePVCName     string
	ArchiveTimeout     string
	BackrestPVCName    string
	PVCName            string
	BackupPVCName      string
	BackupPath         string
	RootSecretName     string
	UserSecretName     string
	PrimarySecretName  string
	SecurityContext    string
	ContainerResources string
	NodeSelector       string
	ConfVolume         string
	CollectAddon       string
	BadgerAddon        string
	//next 2 are for the replica deployment only
	Replicas    string
	PrimaryHost string
}

// ReplicaSuffix ...
const ReplicaSuffix = "-replica"

var strategyMap map[string]Strategy

func init() {
	strategyMap = make(map[string]Strategy)
	strategyMap["1"] = Strategy1{}
}

// AddClusterBase ...
func AddClusterBase(clientset *kubernetes.Clientset, client *rest.RESTClient, cl *crv1.Pgcluster, namespace string) {
	var err error

	if cl.Spec.Status == crv1.UpgradeCompletedStatus {
		log.Warn("crv1 pgcluster " + cl.Spec.ClusterName + " is already marked complete, will not recreate")
		return
	}

	var pvcName string

	_, found, err := kubeapi.GetPVC(clientset, cl.Spec.Name, namespace)
	if found {
		log.Debugf("pvc [%s] already present from previous cluster with this same name, will not recreate\n", cl.Spec.Name)
		pvcName = cl.Spec.Name
	} else {
		pvcName, err = pvc.CreatePVC(clientset, &cl.Spec.PrimaryStorage, cl.Spec.Name, cl.Spec.Name, namespace)
		if err != nil {
			log.Error(err)
			return
		}
		log.Debug("created primary pvc [" + pvcName + "]")
	}

	if cl.Spec.UserLabels["archive"] == "true" {
		pvcName := cl.Spec.Name + "-xlog"
		_, found, err = kubeapi.GetPVC(clientset, pvcName, namespace)
		if found {
			log.Debugf("pvc [%s] already present from previous cluster with this same name, will not recreate\n", pvcName)
		} else {
			_, err := pvc.CreatePVC(clientset, &cl.Spec.PrimaryStorage, pvcName, cl.Spec.Name, namespace)
			if err != nil {
				log.Error(err)
				return
			}
		}
	}
	if cl.Spec.UserLabels[util.LABEL_BACKREST] == "true" {
		pvcName := cl.Spec.Name + "-backrestrepo"
		_, found, err = kubeapi.GetPVC(clientset, pvcName, namespace)
		if found {
			log.Debugf("pvc [%s] already present from previous cluster with this same name, will not recreate\n", pvcName)
		} else {
			storage := crv1.PgStorageSpec{}
			pgoStorage := operator.Pgo.Storage[operator.Pgo.BackupStorage]
			storage.StorageClass = pgoStorage.StorageClass
			storage.AccessMode = pgoStorage.AccessMode
			storage.Size = pgoStorage.Size
			storage.StorageType = pgoStorage.StorageType
			storage.SupplementalGroups = pgoStorage.SupplementalGroups
			storage.Fsgroup = pgoStorage.Fsgroup

			_, err := pvc.CreatePVC(clientset, &storage, pvcName, cl.Spec.Name, namespace)
			if err != nil {
				log.Error(err)
				return
			}
		}
	}

	log.Debug("creating Pgcluster object strategy is [" + cl.Spec.Strategy + "]")
	//allows user to override with their own passwords
	if cl.Spec.Password != "" {
		log.Debug("user has set a password, will use that instead of generated ones or the secret-from settings")
		cl.Spec.RootPassword = cl.Spec.Password
		cl.Spec.Password = cl.Spec.Password
		cl.Spec.PrimaryPassword = cl.Spec.Password
	}

	var err1, err2, err3 error
	if cl.Spec.SecretFrom != "" {
		log.Debug("secret-from is specified! using " + cl.Spec.SecretFrom)
		_, cl.Spec.RootPassword, err1 = util.GetPasswordFromSecret(clientset, namespace, cl.Spec.SecretFrom+crv1.RootSecretSuffix)
		_, cl.Spec.Password, err2 = util.GetPasswordFromSecret(clientset, namespace, cl.Spec.SecretFrom+crv1.UserSecretSuffix)
		_, cl.Spec.PrimaryPassword, err3 = util.GetPasswordFromSecret(clientset, namespace, cl.Spec.SecretFrom+crv1.PrimarySecretSuffix)
		if err1 != nil || err2 != nil || err3 != nil {
			log.Error("error getting secrets using SecretFrom " + cl.Spec.SecretFrom)
			return
		}
	}

	_, _, _, err = createDatabaseSecrets(clientset, client, cl, namespace)
	if err != nil {
		log.Error("error in create secrets " + err.Error())
		return
	}

	if cl.Spec.Strategy == "" {
		cl.Spec.Strategy = "1"
		log.Info("using default strategy")
	}

	strategy, ok := strategyMap[cl.Spec.Strategy]
	if ok {
		log.Info("strategy found")
	} else {
		log.Error("invalid Strategy requested for cluster creation" + cl.Spec.Strategy)
		return
	}

	//replaced with ccpimagetag instead of pg version
	//setFullVersion(client, cl, namespace)

	strategy.AddCluster(clientset, client, cl, namespace, pvcName)

	err = util.Patch(client, "/spec/status", crv1.UpgradeCompletedStatus, crv1.PgclusterResourcePlural, cl.Spec.Name, namespace)
	if err != nil {
		log.Error("error in status patch " + err.Error())
	}
	err = util.Patch(client, "/spec/PrimaryStorage/name", pvcName, crv1.PgclusterResourcePlural, cl.Spec.Name, namespace)
	if err != nil {
		log.Error("error in pvcname patch " + err.Error())
	}

	log.Debugf("before pgpool check [%s]", cl.Spec.UserLabels[util.LABEL_PGPOOL])
	//add pgpool deployment if requested
	if cl.Spec.UserLabels[util.LABEL_PGPOOL] == "true" {
		log.Debug("pgpool requested")
		//create the pgpool deployment using that credential
		AddPgpool(clientset, cl, namespace, true)
	}
	//add pgbouncer deployment if requested
	if cl.Spec.UserLabels[util.LABEL_PGBOUNCER] == "true" {
		log.Debug("pgbouncer requested")
		//create the pgbouncer deployment using that credential
		AddPgbouncer(clientset, cl, namespace, true)
	}

	//add replicas if requested
	if cl.Spec.Replicas != "" {
		replicaCount, err := strconv.Atoi(cl.Spec.Replicas)
		if err != nil {
			log.Error("error in replicas value " + err.Error())
			return
		}
		//create a CRD for each replica
		for i := 0; i < replicaCount; i++ {
			spec := crv1.PgreplicaSpec{}
			//get the resource config
			spec.ContainerResources = cl.Spec.ContainerResources
			//get the storage config
			spec.ReplicaStorage, _ = operator.Pgo.GetStorageSpec(operator.Pgo.ReplicaStorage)

			spec.UserLabels = cl.Spec.UserLabels
			labels := make(map[string]string)
			labels[util.LABEL_PG_CLUSTER] = cl.Spec.Name

			spec.ClusterName = cl.Spec.Name
			uniqueName := util.RandStringBytesRmndr(4)
			labels[util.LABEL_NAME] = cl.Spec.Name + "-" + uniqueName
			spec.Name = labels[util.LABEL_NAME]
			newInstance := &crv1.Pgreplica{
				ObjectMeta: meta_v1.ObjectMeta{
					Name:   labels[util.LABEL_NAME],
					Labels: labels,
				},
				Spec: spec,
				Status: crv1.PgreplicaStatus{
					State:   crv1.PgreplicaStateCreated,
					Message: "Created, not processed yet",
				},
			}
			result := crv1.Pgreplica{}

			err = client.Post().
				Resource(crv1.PgreplicaResourcePlural).
				Namespace(namespace).
				Body(newInstance).
				Do().Into(&result)
			if err != nil {
				log.Error(" in creating Pgreplica instance" + err.Error())
			}

		}
	}

}

// DeleteClusterBase ...
func DeleteClusterBase(clientset *kubernetes.Clientset, client *rest.RESTClient, cl *crv1.Pgcluster, namespace string) {

	log.Debug("deleteCluster called with strategy " + cl.Spec.Strategy)

	aftask := AutoFailoverTask{}
	aftask.Clear(client, cl.Spec.Name, namespace)

	if cl.Spec.Strategy == "" {
		cl.Spec.Strategy = "1"
	}

	strategy, ok := strategyMap[cl.Spec.Strategy]
	if ok {
		log.Info("strategy found")
	} else {
		log.Error("invalid Strategy requested for cluster creation" + cl.Spec.Strategy)
		return
	}

	strategy.DeleteCluster(clientset, client, cl, namespace)

	err := kubeapi.Deletepgupgrade(client, cl.Spec.Name, namespace)
	if err == nil {
		log.Info("deleted pgupgrade " + cl.Spec.Name)
	} else if kerrors.IsNotFound(err) {
		log.Info("will not delete pgupgrade, not found for " + cl.Spec.Name)
	} else {
		log.Error("error deleting pgupgrade " + cl.Spec.Name + err.Error())
	}

}

// AddUpgradeBase ...
func AddUpgradeBase(clientset *kubernetes.Clientset, client *rest.RESTClient, upgrade *crv1.Pgupgrade, namespace string, cl *crv1.Pgcluster) error {
	var err error

	//get the strategy to use
	if cl.Spec.Strategy == "" {
		cl.Spec.Strategy = "1"
		log.Info("using default cluster strategy")
	}

	strategy, ok := strategyMap[cl.Spec.Strategy]
	if ok {
		log.Info("strategy found")
	} else {
		log.Error("invalid Strategy requested for cluster upgrade" + cl.Spec.Strategy)
		return err
	}

	//invoke the strategy
	if upgrade.Spec.UpgradeType == "minor" {
		err = strategy.MinorUpgrade(clientset, client, cl, upgrade, namespace)
		if err == nil {
			err = util.Patch(client, "/spec/upgradestatus", crv1.UpgradeCompletedStatus, crv1.PgupgradeResourcePlural, upgrade.Spec.Name, namespace)
		}
	} else if upgrade.Spec.UpgradeType == "major" {
		err = strategy.MajorUpgrade(clientset, client, cl, upgrade, namespace)
	} else {
		log.Error("invalid UPGRADE_TYPE requested for cluster upgrade" + upgrade.Spec.UpgradeType)
		return err
	}
	if err == nil {
		log.Info("updating the pg version after cluster upgrade")
		fullVersion := upgrade.Spec.CCPImageTag
		err = util.Patch(client, "/spec/ccpimagetag", fullVersion, crv1.PgclusterResourcePlural, upgrade.Spec.Name, namespace)
		if err != nil {
			log.Error(err.Error())
		}
	}

	return err

}

// ScaleBase ...
func ScaleBase(clientset *kubernetes.Clientset, client *rest.RESTClient, replica *crv1.Pgreplica, namespace string) {
	var err error

	if replica.Spec.Status == crv1.UpgradeCompletedStatus {
		log.Warn("crv1 pgreplica " + replica.Spec.Name + " is already marked complete, will not recreate")
		return
	}

	//get the pgcluster CRD to base the replica off of
	cluster := crv1.Pgcluster{}
	_, err = kubeapi.Getpgcluster(client, &cluster,
		replica.Spec.ClusterName, namespace)
	if err != nil {
		return
	}

	//create the PVC
	pvcName, err := pvc.CreatePVC(clientset, &replica.Spec.ReplicaStorage, replica.Spec.Name, cluster.Spec.Name, namespace)
	if err != nil {
		log.Error(err)
		return
	}

	if cluster.Spec.UserLabels[util.LABEL_ARCHIVE] == "true" {
		_, err := pvc.CreatePVC(clientset, &cluster.Spec.PrimaryStorage, replica.Spec.Name+"-xlog", cluster.Spec.Name, namespace)
		if err != nil {
			log.Error(err)
			return
		}
	}

	if cluster.Spec.UserLabels[util.LABEL_BACKREST] == "true" {
		_, err := pvc.CreatePVC(clientset, &cluster.Spec.PrimaryStorage, replica.Spec.Name+"-backrestrepo", cluster.Spec.Name, namespace)
		if err != nil {
			log.Error(err)
			return
		}
	}

	log.Debug("created replica pvc [" + pvcName + "]")

	//update the replica CRD pvcname
	err = util.Patch(client, "/spec/replicastorage/name", pvcName, crv1.PgreplicaResourcePlural, replica.Spec.Name, namespace)
	if err != nil {
		log.Error("error in pvcname patch " + err.Error())
	}

	log.Debug("creating Pgreplica object strategy is [" + cluster.Spec.Strategy + "]")

	if cluster.Spec.Strategy == "" {
		log.Info("using default strategy")
	}

	strategy, ok := strategyMap[cluster.Spec.Strategy]
	if ok {
		log.Info("strategy found")
	} else {
		log.Error("invalid Strategy requested for replica creation" + cluster.Spec.Strategy)
		return
	}

	//create the replica service if it doesnt exist
	serviceName := replica.Spec.ClusterName + "-replica"
	serviceFields := ServiceTemplateFields{
		Name:        serviceName,
		ClusterName: replica.Spec.ClusterName,
		Port:        cluster.Spec.Port,
		ServiceType: operator.Pgo.Cluster.ServiceType,
	}

	err = CreateService(clientset, &serviceFields, namespace)
	if err != nil {
		log.Error(err)
		return
	}

	//instantiate the replica
	strategy.Scale(clientset, client, replica, namespace, pvcName, &cluster)

	//update the replica CRD status
	err = util.Patch(client, "/spec/status", crv1.UpgradeCompletedStatus, crv1.PgreplicaResourcePlural, replica.Spec.Name, namespace)
	if err != nil {
		log.Error("error in status patch " + err.Error())
	}

}

// ScaleDownBase ...
func ScaleDownBase(clientset *kubernetes.Clientset, client *rest.RESTClient, replica *crv1.Pgreplica, namespace string) {
	var err error

	//get the pgcluster CRD for this replica
	cluster := crv1.Pgcluster{}
	_, err = kubeapi.Getpgcluster(client, &cluster,
		replica.Spec.ClusterName, namespace)
	if err != nil {
		return
	}

	log.Debug("creating Pgreplica object strategy is [" + cluster.Spec.Strategy + "]")

	if cluster.Spec.Strategy == "" {
		log.Info("using default strategy")
	}

	strategy, ok := strategyMap[cluster.Spec.Strategy]
	if ok {
		log.Info("strategy found")
	} else {
		log.Error("invalid Strategy requested for replica creation" + cluster.Spec.Strategy)
		return
	}

	strategy.DeleteReplica(clientset, replica, namespace)

}

/**
import (
	log "github.com/Sirupsen/logrus"
	crv1 "github.com/crunchydata/postgres-operator/apis/cr/v1"
	msgs "github.com/crunchydata/postgres-operator/apiservermsgs"
	"github.com/crunchydata/postgres-operator/kubeapi"
	"k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	//"k8s.io/client-go/rest"
	"math/rand"
	"strings"
	"time"
)

*/
// createDatabaseSecrets create pgroot, pgprimary, and pguser secrets
func createDatabaseSecrets(clientset *kubernetes.Clientset, restclient *rest.RESTClient, cl *crv1.Pgcluster, namespace string) (string, string, string, error) {

	//pgroot
	username := "postgres"
	suffix := crv1.RootSecretSuffix

	var secretName string
	var err error

	secretName = cl.Spec.Name + suffix
	pgPassword := util.GeneratePassword(10)
	if cl.Spec.RootPassword != "" {
		log.Debug("using user specified password for secret " + secretName)
		pgPassword = cl.Spec.RootPassword
	}

	err = util.CreateSecret(clientset, cl.Spec.Name, secretName, username, pgPassword, namespace)
	if err != nil {
		log.Error("error creating secret" + err.Error())
	}

	cl.Spec.RootSecretName = secretName
	err = util.Patch(restclient, "/spec/rootsecretname", secretName, crv1.PgclusterResourcePlural, cl.Spec.Name, namespace)
	if err != nil {
		log.Error("error patching cluster" + err.Error())
	}

	///primary
	username = "primaryuser"
	suffix = crv1.PrimarySecretSuffix

	secretName = cl.Spec.Name + suffix
	primaryPassword := util.GeneratePassword(10)
	if cl.Spec.PrimaryPassword != "" {
		log.Debug("using user specified password for secret " + secretName)
		primaryPassword = cl.Spec.PrimaryPassword
	}

	err = util.CreateSecret(clientset, cl.Spec.Name, secretName, username, primaryPassword, namespace)
	if err != nil {
		log.Error("error creating secret2" + err.Error())
	}

	cl.Spec.PrimarySecretName = secretName
	err = util.Patch(restclient, "/spec/primarysecretname", secretName, crv1.PgclusterResourcePlural, cl.Spec.Name, namespace)
	if err != nil {
		log.Error("error patching cluster " + err.Error())
	}

	///pguser
	username = "testuser"
	suffix = crv1.UserSecretSuffix

	secretName = cl.Spec.Name + suffix
	testPassword := util.GeneratePassword(10)
	if cl.Spec.Password != "" {
		log.Debug("using user specified password for secret " + secretName)
		testPassword = cl.Spec.Password
	}

	err = util.CreateSecret(clientset, cl.Spec.Name, secretName, username, testPassword, namespace)
	if err != nil {
		log.Error("error creating secret " + err.Error())
	}

	cl.Spec.UserSecretName = secretName
	err = util.Patch(restclient, "/spec/usersecretname", secretName, crv1.PgclusterResourcePlural, cl.Spec.Name, namespace)
	if err != nil {
		log.Error("error patching cluster " + err.Error())
	}

	return pgPassword, primaryPassword, testPassword, err
}