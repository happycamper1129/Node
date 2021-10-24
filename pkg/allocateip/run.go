package allocateip

import (
	"context"
	"fmt"
	"os"
	"time"

	v3 "github.com/projectcalico/libcalico-go/lib/apis/v3"
	client "github.com/projectcalico/libcalico-go/lib/clientv3"
	cerrors "github.com/projectcalico/libcalico-go/lib/errors"
	"github.com/projectcalico/libcalico-go/lib/ipam"
	"github.com/projectcalico/libcalico-go/lib/net"
	"github.com/projectcalico/libcalico-go/lib/options"
	"github.com/projectcalico/node/pkg/calicoclient"
	"github.com/projectcalico/typha/pkg/logutils"
	"github.com/sirupsen/logrus"
)

// This file contains the main processing and common logic for assigning tunnel addresses,
// used by calico/node to set the host's tunnel address if IPIP or VXLAN is enabled.
// It will assign an address address if there are any available, and remove any tunnel address
// that is configured if it should no longer be.

func Run() {
	// Log to stdout.  this prevents our logs from being interpreted as errors by, for example,
	// fluentd's default configuration.
	logrus.SetOutput(os.Stdout)

	// Set log formatting.
	logrus.SetFormatter(&logutils.Formatter{})

	// Install a hook that adds file and line number information.
	logrus.AddHook(&logutils.ContextHook{})

	// Load the client config from environment.
	_, c := calicoclient.CreateClient()

	// This binary is only ever invoked _after_ the
	// startup binary has been invoked and the modified environments have
	// been sourced.  Therefore, the NODENAME environment will always be
	// set at this point.
	nodename := os.Getenv("NODENAME")
	if nodename == "" {
		logrus.Panic("NODENAME environment is not set")
	}

	ctx := context.Background()
	// Get node resource for given nodename.
	node, err := c.Nodes().Get(ctx, nodename, options.GetOptions{})
	if err != nil {
		logrus.WithError(err).Fatalf("failed to fetch node resource '%s'", nodename)
	}

	// Get list of ip pools
	ipPoolList, err := c.IPPools().List(ctx, options.ListOptions{})
	if err != nil {
		logrus.WithError(err).Fatal("Unable to query IP pool configuration")
	}

	// Query the IPIP enabled pools and either configure the tunnel
	// address, or remove it.
	if cidrs := determineIPIPEnabledPoolCIDRs(*node, *ipPoolList); len(cidrs) > 0 {
		ensureHostTunnelAddress(ctx, c, nodename, cidrs, false)
	} else {
		removeHostTunnelAddr(ctx, c, nodename, false)
	}

	// Query the VXLAN enabled pools and either configure the tunnel
	// address, or remove it.
	if cidrs := determineVXLANEnabledPoolCIDRs(*node, *ipPoolList); len(cidrs) > 0 {
		ensureHostTunnelAddress(ctx, c, nodename, cidrs, true)
	} else {
		removeHostTunnelAddr(ctx, c, nodename, true)
	}
}

func ensureHostTunnelAddress(ctx context.Context, c client.Interface, nodename string, cidrs []net.IPNet, vxlan bool) {
	logCtx := getLogger(vxlan)
	logCtx.WithField("Node", nodename).Debug("Ensure tunnel address is set")

	// Get the currently configured address.
	node, err := c.Nodes().Get(ctx, nodename, options.GetOptions{})
	if err != nil {
		logCtx.WithError(err).Fatalf("Unable to retrieve tunnel address. Error getting node '%s'", nodename)
	}

	// Get the address
	var addr string
	if vxlan {
		addr = node.Spec.IPv4VXLANTunnelAddr
	} else if node.Spec.BGP != nil {
		addr = node.Spec.BGP.IPv4IPIPTunnelAddr
	}

	if addr == "" {
		// The tunnel has no IP address assigned, assign one.
		logCtx.Debug("tunnel is not assigned - assign IP")
		assignHostTunnelAddr(ctx, c, nodename, cidrs, vxlan)
	} else if isIpInPool(addr, cidrs) {
		// The tunnel address is still valid, so leave as it.
		logCtx.WithField("IP", addr).Info("tunnel address is still valid")
	} else {
		// The address that is currently assigned is no longer part
		// of an encapsulatin-enabled pool, so release the IP, and reassign.
		logCtx.WithField("IP", addr).Info("Reassigning tunnel address")
		ipAddr := net.ParseIP(addr)
		if err != nil {
			logCtx.WithError(err).Fatalf("Failed to parse the CIDR '%s'", addr)
		}

		ipsToRelease := []net.IP{*ipAddr}
		_, err := c.IPAM().ReleaseIPs(ctx, ipsToRelease)
		if err != nil {
			logCtx.WithField("IP", ipAddr.String()).WithError(err).Fatal("Error releasing address")
		}

		// Assign a new tunnel address.
		assignHostTunnelAddr(ctx, c, nodename, cidrs, vxlan)
	}
}

// assignHostTunnelAddr claims an IP address from the first pool
// with some space. Stores the result in the host's config as its tunnel
// address. It will assign a VXLAN address if vxlan is true, otherwise an IPIP address.
func assignHostTunnelAddr(ctx context.Context, c client.Interface, nodename string, cidrs []net.IPNet, vxlan bool) {
	// Build attributes and handle for this allocation.
	attrs := map[string]string{ipam.AttributeNode: nodename}
	var handle string
	if vxlan {
		attrs[ipam.AttributeType] = "vxlanTunnelAddress"
		handle = fmt.Sprintf("vxlan-tunnel-addr-%s", nodename)
	} else {
		attrs[ipam.AttributeType] = "ipipTunnelAddress"
		handle = fmt.Sprintf("ipip-tunnel-addr-%s", nodename)
	}
	logCtx := getLogger(vxlan)

	args := ipam.AutoAssignArgs{
		Num4:      1,
		Num6:      0,
		HandleID:  &handle,
		Attrs:     attrs,
		Hostname:  nodename,
		IPv4Pools: cidrs,
	}

	ipv4Addrs, _, err := c.IPAM().AutoAssign(ctx, args)
	if err != nil {
		logCtx.WithError(err).Fatal("Unable to autoassign an address")
	}

	if len(ipv4Addrs) == 0 {
		logCtx.Fatal("Unable to autoassign an address - pools are likely exhausted.")
	}

	var updateError error
	// If the update fails with ResourceConflict error then retry 5 times with 1 second delay before failing.
	for i := 0; i < 5; i++ {
		node, err := c.Nodes().Get(ctx, nodename, options.GetOptions{})
		if err != nil {
			logCtx.WithError(err).Fatalf("Unable to retrieve tunnel address for cleanup. Error getting node '%s'", nodename)
		}

		if vxlan {
			node.Spec.IPv4VXLANTunnelAddr = ipv4Addrs[0].IP.String()
		} else {
			if node.Spec.BGP == nil {
				node.Spec.BGP = &v3.NodeBGPSpec{}
			}
			node.Spec.BGP.IPv4IPIPTunnelAddr = ipv4Addrs[0].IP.String()
		}

		_, updateError = c.Nodes().Update(ctx, node, options.SetOptions{})
		if _, ok := updateError.(cerrors.ErrorResourceUpdateConflict); ok {
			// Wait for a second and try again if there was a conflict during the resource update.
			logCtx.Infof("Error updating node %s: %s. Retrying.", node.Name, err)
			time.Sleep(1 * time.Second)
			continue
		}

		break
	}

	// Check to see if there was still an error after the retry loop,
	// and release the IP if there was an error.
	if updateError != nil {
		// We hit an error, so release the IP address before exiting.
		_, err := c.IPAM().ReleaseIPs(ctx, []net.IP{{IP: ipv4Addrs[0].IP}})
		if err != nil {
			logCtx.WithError(err).WithField("IP", ipv4Addrs[0].IP.String()).Errorf("Error releasing IP address on failure")
		}

		// Log the error and exit with exit code 1.
		logCtx.WithError(err).WithField("IP", ipv4Addrs[0].IP.String()).Fatal("Unable to set tunnel address")
	}

	logCtx.WithField("IP", ipv4Addrs[0].String()).Info("Set tunnel address")
}

// removeHostTunnelAddr removes any existing IP address for this host's
// tunnel device and releases the IP from IPAM.  If no IP is assigned this function
// is a no-op.
func removeHostTunnelAddr(ctx context.Context, c client.Interface, nodename string, vxlan bool) {
	var updateError error
	logCtx := getLogger(vxlan)

	// If the update fails with ResourceConflict error then retry 5 times with 1 second delay before failing.
	for i := 0; i < 5; i++ {
		node, err := c.Nodes().Get(ctx, nodename, options.GetOptions{})
		if err != nil {
			logCtx.WithError(err).Fatalf("Unable to retrieve tunnel address for cleanup. Error getting node '%s'", nodename)
		}

		// Determine if we need to do any work.
		ipipTunnelAddrExists := (node.Spec.BGP != nil && node.Spec.BGP.IPv4IPIPTunnelAddr != "")
		vxlanTunnelAddrExists := node.Spec.IPv4VXLANTunnelAddr != ""
		if (vxlan && !vxlanTunnelAddrExists) || (!vxlan && !ipipTunnelAddrExists) {
			logCtx.Debug("No tunnel address assigned, and not required")
			return
		}

		// Find out the currently assigned address and remove it from the node.
		var ipAddr *net.IP
		if vxlan {
			ipAddr = net.ParseIP(node.Spec.IPv4VXLANTunnelAddr)
			node.Spec.IPv4VXLANTunnelAddr = ""
		} else if node.Spec.BGP != nil {
			ipAddr = net.ParseIP(node.Spec.BGP.IPv4IPIPTunnelAddr)
			node.Spec.BGP.IPv4IPIPTunnelAddr = ""
		}

		// Release the IP.
		if _, err := c.IPAM().ReleaseIPs(ctx, []net.IP{*ipAddr}); err != nil {
			logCtx.WithError(err).WithField("IP", ipAddr.String()).Fatal("Error releasing address from IPAM")
		}

		// Update the node object.
		_, updateError = c.Nodes().Update(ctx, node, options.SetOptions{})
		if _, ok := updateError.(cerrors.ErrorResourceUpdateConflict); ok {
			// Wait for a second and try again if there was a conflict during the resource update.
			logCtx.Infof("Error updating node %s: %s. Retrying.", node.Name, err)
			time.Sleep(1 * time.Second)
			continue
		}

		break
	}

	// Check to see if there was still an error after the retry loop,
	// and log and exit if there was an error.
	if updateError != nil {
		// We hit an error, so release the IP address before exiting.
		// Log the error and exit with exit code 1.
		logCtx.WithError(updateError).Fatal("Unable to remove tunnel address")
	}
}

// isIpInPool returns if the IP address is in one of the supplied pools.
func isIpInPool(ipAddrStr string, cidrs []net.IPNet) bool {
	ipAddress := net.ParseIP(ipAddrStr)
	for _, cidr := range cidrs {
		if cidr.Contains(ipAddress.IP) {
			return true
		}
	}
	return false
}

func getLogger(vxlan bool) *logrus.Entry {
	if vxlan {
		return logrus.WithField("type", "vxlanTunnelAddress")
	} else {
		return logrus.WithField("type", "ipipTunnelAddress")
	}
}
