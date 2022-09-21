package manifests

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	operv1 "github.com/openshift/api/operator/v1"
	hiveext "github.com/openshift/assisted-service/api/hiveextension/v1beta1"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	"github.com/openshift/installer/pkg/asset"
	"github.com/openshift/installer/pkg/asset/agent"
	"github.com/openshift/installer/pkg/ipnet"
	"github.com/openshift/installer/pkg/types"
	"github.com/openshift/installer/pkg/types/defaults"
	"github.com/openshift/installer/pkg/validate"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

var (
	agentClusterInstallFilename = filepath.Join(clusterManifestDir, "agent-cluster-install.yaml")
)

// AgentClusterInstall generates the agent-cluster-install.yaml file.
type AgentClusterInstall struct {
	File   *asset.File
	Config *hiveext.AgentClusterInstall
}

var _ asset.WritableAsset = (*AgentClusterInstall)(nil)

// Name returns a human friendly name for the asset.
func (*AgentClusterInstall) Name() string {
	return "AgentClusterInstall Config"
}

// Dependencies returns all of the dependencies directly needed to generate
// the asset.
func (*AgentClusterInstall) Dependencies() []asset.Asset {
	return []asset.Asset{
		&agent.OptionalInstallConfig{},
	}
}

// Generate generates the AgentClusterInstall manifest.
func (a *AgentClusterInstall) Generate(dependencies asset.Parents) error {
	installConfig := &agent.OptionalInstallConfig{}
	dependencies.Get(installConfig)

	if installConfig.Config != nil {
		var numberOfWorkers int = 0
		for _, compute := range installConfig.Config.Compute {
			numberOfWorkers = numberOfWorkers + int(*compute.Replicas)
		}

		clusterNetwork := []hiveext.ClusterNetworkEntry{}
		for _, cn := range installConfig.Config.Networking.ClusterNetwork {
			_, cidr, err := net.ParseCIDR(cn.CIDR.String())
			if err != nil {
				return errors.Wrap(err, "failed to parse ClusterNetwork CIDR")
			}
			err = validate.SubnetCIDR(cidr)
			if err != nil {
				return errors.Wrap(err, "failed to validate ClusterNetwork CIDR")
			}

			entry := hiveext.ClusterNetworkEntry{
				CIDR:       cidr.String(),
				HostPrefix: cn.HostPrefix,
			}
			clusterNetwork = append(clusterNetwork, entry)
		}

		serviceNetwork := []string{}
		for _, sn := range installConfig.Config.Networking.ServiceNetwork {
			cidr, err := ipnet.ParseCIDR(sn.String())
			if err != nil {
				return errors.Wrap(err, "failed to parse ServiceNetwork CIDR")
			}
			serviceNetwork = append(serviceNetwork, cidr.String())
		}

		agentClusterInstall := &hiveext.AgentClusterInstall{
			ObjectMeta: metav1.ObjectMeta{
				Name:      getAgentClusterInstallName(installConfig),
				Namespace: getObjectMetaNamespace(installConfig),
			},
			Spec: hiveext.AgentClusterInstallSpec{
				ImageSetRef: &hivev1.ClusterImageSetReference{
					Name: getClusterImageSetReferenceName(),
				},
				ClusterDeploymentRef: corev1.LocalObjectReference{
					Name: getClusterDeploymentName(installConfig),
				},
				Networking: hiveext.Networking{
					ClusterNetwork: clusterNetwork,
					ServiceNetwork: serviceNetwork,
				},
				SSHPublicKey: strings.Trim(installConfig.Config.SSHKey, "|\n\t"),
				ProvisionRequirements: hiveext.ProvisionRequirements{
					ControlPlaneAgents: int(*installConfig.Config.ControlPlane.Replicas),
					WorkerAgents:       numberOfWorkers,
				},
			},
		}

		setNetworkType(agentClusterInstall, installConfig.Config, "NetworkType is not specified in InstallConfig.")

		// TODO: Handle the case where both IPv4 and IPv6 VIPs are specified
		apiVIP, ingressVIP := getVIPs(&installConfig.Config.Platform)

		// set APIVIP and IngressVIP only for non SNO cluster for Baremetal and Vsphere platforms
		// SNO cluster is determined by number of ControlPlaneAgents which should be 1
		if int(*installConfig.Config.ControlPlane.Replicas) > 1 && apiVIP != "" && ingressVIP != "" {
			agentClusterInstall.Spec.APIVIP = apiVIP
			agentClusterInstall.Spec.IngressVIP = ingressVIP
		}

		a.Config = agentClusterInstall

		agentClusterInstallData, err := yaml.Marshal(agentClusterInstall)
		if err != nil {
			return errors.Wrap(err, "failed to marshal agent installer AgentClusterInstall")
		}

		a.File = &asset.File{
			Filename: agentClusterInstallFilename,
			Data:     agentClusterInstallData,
		}
	}
	return a.finish()
}

// Files returns the files generated by the asset.
func (a *AgentClusterInstall) Files() []*asset.File {
	if a.File != nil {
		return []*asset.File{a.File}
	}
	return []*asset.File{}
}

// Load returns agentclusterinstall asset from the disk.
func (a *AgentClusterInstall) Load(f asset.FileFetcher) (bool, error) {

	agentClusterInstallFile, err := f.FetchByName(agentClusterInstallFilename)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, errors.Wrap(err, fmt.Sprintf("failed to load %s file", agentClusterInstallFilename))
	}

	a.File = agentClusterInstallFile

	agentClusterInstall := &hiveext.AgentClusterInstall{}
	if err := yaml.UnmarshalStrict(agentClusterInstallFile.Data, agentClusterInstall); err != nil {
		err = errors.Wrapf(err, "failed to unmarshal %s", agentClusterInstallFilename)
		return false, err
	}

	setNetworkType(agentClusterInstall, &types.InstallConfig{}, "NetworkType is not specified in AgentClusterInstall.")

	a.Config = agentClusterInstall

	if err = a.finish(); err != nil {
		return false, err
	}
	return true, nil
}

func (a *AgentClusterInstall) finish() error {

	if a.Config == nil {
		return errors.New("missing configuration or manifest file")
	}

	if err := a.validateIPAddressAndNetworkType().ToAggregate(); err != nil {
		return errors.Wrapf(err, "invalid NetworkType configured")
	}

	return nil
}

// Sets the default network type to OVNKubernetes if it is unspecified in the
// AgentClusterInstall or InstallConfig
func setNetworkType(aci *hiveext.AgentClusterInstall, installConfig *types.InstallConfig,
	warningMessage string) {

	if aci.Spec.Networking.NetworkType != "" {
		return
	}

	if installConfig != nil && installConfig.Networking != nil &&
		installConfig.Networking.NetworkType != "" {
		aci.Spec.Networking.NetworkType = installConfig.NetworkType
		return
	}

	defaults.SetInstallConfigDefaults(installConfig)
	logrus.Infof("%s Defaulting NetworkType to %s.", warningMessage, installConfig.NetworkType)
	aci.Spec.Networking.NetworkType = installConfig.NetworkType
}

func isIPv6(ipAddress net.IP) bool {
	ip := ipAddress.To16()
	return ip != nil
}

func (a *AgentClusterInstall) validateIPAddressAndNetworkType() field.ErrorList {
	allErrs := field.ErrorList{}

	fieldPath := field.NewPath("spec", "networking", "networkType")
	clusterNetworkPath := field.NewPath("spec", "networking", "clusterNetwork")
	serviceNetworkPath := field.NewPath("spec", "networking", "serviceNetwork")

	if a.Config.Spec.Networking.NetworkType == string(operv1.NetworkTypeOpenShiftSDN) {
		hasIPv6 := false
		for _, cn := range a.Config.Spec.Networking.ClusterNetwork {
			ip, _, errCIDR := net.ParseCIDR(cn.CIDR)
			if errCIDR != nil {
				allErrs = append(allErrs, field.Required(clusterNetworkPath, "error parsing the clusterNetwork CIDR"))
			}
			if isIPv6(ip) {
				hasIPv6 = true
			}
		}
		if hasIPv6 {
			allErrs = append(allErrs, field.Required(fieldPath,
				fmt.Sprintf("clusterNetwork CIDR is IPv6 and is not compatible with networkType %s",
					operv1.NetworkTypeOpenShiftSDN)))
		}

		hasIPv6 = false
		for _, cidr := range a.Config.Spec.Networking.ServiceNetwork {
			ip, _, errCIDR := net.ParseCIDR(cidr)
			if errCIDR != nil {
				allErrs = append(allErrs, field.Required(serviceNetworkPath, "error parsing the clusterNetwork CIDR"))
			}
			if isIPv6(ip) {
				hasIPv6 = true
			}
		}
		if hasIPv6 {
			allErrs = append(allErrs, field.Required(fieldPath,
				fmt.Sprintf("serviceNetwork CIDR is IPv6 and is not compatible with networkType %s",
					operv1.NetworkTypeOpenShiftSDN)))
		}
	}

	return allErrs
}
