package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/davecgh/go-spew/spew"
	koko "github.com/redhat-nfvpe/koko/api"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"strings"

	mpb "github.com/networkop/meshnet-cni/daemon/proto/meshnet/v1beta1"
	"github.com/networkop/meshnet-cni/utils/wireutil"
)

const (
	vxlanBase             = 5000
	defaultPort           = "51111"
	localhost             = "localhost"
	localDaemon           = localhost + ":" + defaultPort
	macvlanMode           = netlink.MACVLAN_MODE_BRIDGE
	INTER_NODE_LINK_VXLAN = "VXLAN"
	INTER_NODE_LINK_GRPC  = "GRPC"
)

var interNodeLinkType = INTER_NODE_LINK_VXLAN

type netConf struct {
	types.NetConf
	Delegate map[string]interface{} `json:"delegate"`
}

type k8sArgs struct {
	types.CommonArgs
	K8S_POD_NAME               types.UnmarshallableString
	K8S_POD_NAMESPACE          types.UnmarshallableString
	K8S_POD_INFRA_CONTAINER_ID types.UnmarshallableString
}

// -------------------------------------------------------------------------------------------------
func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

// -------------------------------------------------------------------------------------------------
// loadConf loads information from cni.conf
func loadConf(bytes []byte) (*netConf, *current.Result, error) {
	n := &netConf{}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, nil, fmt.Errorf("failed to load netconf: %v", err)
	}

	// Parse previous result.
	if n.RawPrevResult == nil {
		// return early if there was no previous result, which is allowed for DEL calls
		return n, &current.Result{}, nil
	}

	// Parse previous result.
	var result *current.Result
	var err error
	if err = version.ParsePrevResult(&n.NetConf); err != nil {
		return nil, nil, fmt.Errorf("could not parse prevResult: %v", err)
	}

	result, err = current.NewResultFromResult(n.PrevResult)
	if err != nil {
		return nil, nil, fmt.Errorf("could not convert result to current version: %v", err)
	}
	return n, result, nil
}

// getVxlanSource uses netlink to get the iface reliably given an IP address.
// when IP and Interface both are present then Interface is going to take preference
// nodeIntf is specified by the user and it's not auto discovered. The user has to be careful that the peer is reachable though this interface otherwise, VxLAN may not work.
// daemonset.yaml meshnet container env required for host_intf override
//          env:
//            - name: HOST_INTF
//   		value: breth2
func getVxlanSource(nodeIP string, nodeIntf string) (string, string, error) {
    if nodeIntf == "" && nodeIP == "" {
        return "", "", fmt.Errorf("meshnetd provided no HOST_IP address: %s or HOST_INTF: %s", nodeIP, nodeIntf)
    }
    nIP := net.ParseIP(nodeIP)
    if nIP == nil && nodeIntf == "" {
        return "", "", fmt.Errorf("parsing failed for meshnetd provided no HOST_IP address: %s and node HOST_INTF: %s", nodeIP, nodeIntf)
    }
    ifaces, _ := net.Interfaces()
    for _, i := range ifaces {
        addrs, _ := i.Addrs()
        for _, a := range addrs {
            var ip net.IP
            switch v := a.(type) {
            case *net.IPNet:
                ip = v.IP
            case *net.IPAddr:
                ip = v.IP
            }
            if nodeIntf != "" {
               if i.Name == nodeIntf {
                   return ip.String(), nodeIntf, nil
               }
            }
            if nIP != nil && nIP.Equal(ip) {
                log.Infof("Found iface %s for address %s", i.Name, nodeIP)
                return nodeIP, i.Name, nil
            }
        }
    }
    return "", "", fmt.Errorf("no iface found for address %s", nodeIP)
}

// -------------------------------------------------------------------------------------------------
// makeVeth creates koko.Veth from NetNS and LinkName
func makeVeth(netNS, linkName string, ip string) (*koko.VEth, error) {
	log.Infof("Creating Veth struct with NetNS:%s and intfName: %s, IP:%s", netNS, linkName, ip)
	veth := koko.VEth{}
	veth.NsName = netNS
	veth.LinkName = linkName
	if ip != "" {
		ipAddr, ipSubnet, err := net.ParseCIDR(ip)
		if err != nil {
			return nil, fmt.Errorf("failed to parse CIDR %s: %s", ip, err)
		}
		veth.IPAddr = []net.IPNet{{
			IP:   ipAddr,
			Mask: ipSubnet.Mask,
		}}
	}
	return &veth, nil
}

// -------------------------------------------------------------------------------------------------
// Creates koko.Vxlan from ParentIF, destination IP and VNI
func makeVxlan(srcIntf string, peerIP string, idx int64) *koko.VxLan {
	return &koko.VxLan{
		ParentIF: srcIntf,
		IPAddr:   net.ParseIP(peerIP),
		ID:       int(vxlanBase + idx),
	}
}

// -------------------------------------------------------------------------------------------------
// Adds interfaces to a POD
func cmdAdd(args *skel.CmdArgs) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log.Info("Parsing cni .conf file")
	n, result, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	log.Info("Parsing CNI_ARGS environment variable")
	cniArgs := k8sArgs{}
	if err := types.LoadArgs(args.Args, &cniArgs); err != nil {
		return err
	}
	log.Infof("Processing ADD POD %s in namespace %s", cniArgs.K8S_POD_NAME, cniArgs.K8S_POD_NAMESPACE)

	log.Infof("Attempting to connect to local meshnet daemon")
	conn, err := grpc.Dial(localDaemon, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Errorf("Failed to connect to local meshnetd on %s", localDaemon)
		return err
	}
	defer conn.Close()

	meshnetClient := mpb.NewLocalClient(conn)

	log.Infof("Add: Retrieving local pod information from meshnet daemon")
	localPod, err := meshnetClient.Get(ctx, &mpb.PodQuery{
		Name:   string(cniArgs.K8S_POD_NAME),
		KubeNs: string(cniArgs.K8S_POD_NAMESPACE),
	})
	if err != nil {
		log.Errorf("Add: Pod %s:%s was not a topology pod returning", string(cniArgs.K8S_POD_NAMESPACE), string(cniArgs.K8S_POD_NAME))
		return types.PrintResult(result, n.CNIVersion)
	}

	// Finding the source IP and interface for VXLAN VTEP
	srcIP, srcIntf, err := getVxlanSource(localPod.NodeIp, localPod.NodeIntf)
	if err != nil {
		return err
	}
	//log.Infof("VxLan route is via %s@%s", srcIP, srcIntf)

	// Marking pod as "alive" by setting its srcIP and NetNS and ContainerId
	localPod.NetNs = args.Netns
	localPod.SrcIp = srcIP
	localPod.ContainerId = args.ContainerID
	log.Infof("Add: Setting pod alive status on meshnet daemon")
	ok, err := meshnetClient.SetAlive(ctx, localPod)
	if err != nil || !ok.Response {
		log.Errorf("Add: Failed to set pod alive status")
		return err
	}

	log.Info("Add: Starting to traverse all links")
	for _, link := range localPod.Links { // Iterate over each link of the local pod
		// Build koko's veth struct for local intf
		myVeth, err := makeVeth(args.Netns, link.LocalIntf, link.LocalIp)
		if err != nil {
			return err
		}

		// First option is macvlan interface
		if link.PeerPod == localhost {
			log.Infof("Peer link is MacVlan")
			macVlan := koko.MacVLan{
				ParentIF: link.PeerIntf,
				Mode:     macvlanMode,
			}
			if err = koko.MakeMacVLan(*myVeth, macVlan); err != nil {
				log.Errorf("Failed to add macvlan interface")
				return err
			}
			log.Infof("macvlan interfacee %s@%s has been added", link.LocalIntf, link.PeerIntf)
			continue
		}

		// Initialising peer pod's metadata
		log.Infof("Add: Pod %s is retrieving peer pod %s information from meshnet daemon", cniArgs.K8S_POD_NAME, link.PeerPod)
		peerPod, err := meshnetClient.Get(ctx, &mpb.PodQuery{
			Name:   link.PeerPod,
			KubeNs: string(cniArgs.K8S_POD_NAMESPACE),
		})
		if err != nil {
			log.Errorf("Add: Failed to retrieve peer pod %s:%s topology", string(cniArgs.K8S_POD_NAMESPACE), link.PeerPod)
			return err
		}

		isAlive := peerPod.SrcIp != "" && peerPod.NetNs != ""
		log.Infof("Add: Is peer pod %s alive?: %t, local pod %s", peerPod.Name, isAlive, localPod.Name)

		if isAlive { // This means we're coming up AFTER our peer so things are pretty easy
			log.Infof("Add: Peer pod %s is alive, local pod %s", peerPod.Name, localPod.Name)
			if peerPod.SrcIp == localPod.SrcIp { // This means we're on the same host
				log.Infof("Add: %s and %s are on the same host", localPod.Name, peerPod.Name)
				// Creating koko's Veth struct for peer intf
				peerVeth, err := makeVeth(peerPod.NetNs, link.PeerIntf, link.PeerIp)
				if err != nil {
					log.Errorf("Add: Failed to build koko Veth struct local pod %s and peer pod %s", localPod.Name, peerPod.Name)
					return err
				}

				// Checking if interfaces already exist
				iExist, _ := koko.IsExistLinkInNS(myVeth.NsName, myVeth.LinkName)
				pExist, _ := koko.IsExistLinkInNS(peerVeth.NsName, peerVeth.LinkName)

				log.Infof("Does the link already exist? local pod %s and peer pod %s, Local:%t, Peer:%t", localPod.Name, peerPod.Name, iExist, pExist)
				if iExist && pExist { // If both link exist, we don't need to do anything
					log.Infof("Both interfaces already exist in namespace local pod %s and peer pod %s", localPod.Name, peerPod.Name)
				} else if !iExist && pExist { // If only peer link exists, we need to destroy it first
					log.Infof("Only peer link exists, removing it first local pod %s and peer pod %s", localPod.Name, peerPod.Name)
					if err := peerVeth.RemoveVethLink(); err != nil {
						log.Errorf("Failed to remove a stale interface %s of my peer %s", peerVeth.LinkName, link.PeerPod)
						return err
					}
					log.Infof("Adding the new veth link to both pods local pod %s and peer pod %s", localPod.Name, peerPod.Name)
					if err = koko.MakeVeth(*myVeth, *peerVeth); err != nil {
						log.Errorf("Error creating VEth pair after peer link remove: %s, local pod %s and peer pod %s", err, localPod.Name, peerPod.Name)
						return err
					}
				} else if iExist && !pExist { // If only local link exists, we need to destroy it first
					log.Infof("Only local link exists, removing it first local pod %s and peer pod %s", localPod.Name, peerPod.Name)
					if err := myVeth.RemoveVethLink(); err != nil {
						log.Errorf("Failed to remove a local stale VEth interface %s for pod %s", myVeth.LinkName, localPod.Name)
						return err
					}
					log.Infof("Adding the new veth link to both pods local pod %s and peer pod %s", localPod.Name, peerPod.Name)
					if err = koko.MakeVeth(*myVeth, *peerVeth); err != nil {
						log.Errorf("Error creating VEth pair after local link remove: %s, local pod %s and peer pod %s", err, localPod.Name, peerPod.Name)
						return err
					}
				} else { // if neither link exists, we have two options
					log.Infof("Neither link exists. Checking if we've been skipped local pod %s and peer pod %s", localPod.Name, peerPod.Name)
					isSkipped, err := meshnetClient.IsSkipped(ctx, &mpb.SkipQuery{
						Pod:    localPod.Name,
						Peer:   peerPod.Name,
						KubeNs: string(cniArgs.K8S_POD_NAMESPACE),
					})
					if err != nil {
						log.Errorf("Local pod %s Failed to read skipped status from our peer %s", localPod.Name, peerPod.Name)
						return err
					}
					log.Infof("Have we %s been skipped by our peer %s? %t", localPod.Name, peerPod.Name, isSkipped.Response)

					// Comparing names to determine higher priority
					higherPrio := localPod.Name > peerPod.Name
					log.Infof("DO we %s have a higher priority than peer %s ? %t", localPod.Name, peerPod.Name, higherPrio)

					if isSkipped.Response || higherPrio { // If peer POD skipped us (booted before us) or we have a higher priority
						log.Infof("Peer POD %s has skipped us or we %s have a higher priority", peerPod.Name, localPod.Name)
						if err = koko.MakeVeth(*myVeth, *peerVeth); err != nil {
							log.Errorf("Error when creating a new VEth pair with koko: %s", err)
							log.Infof("local pod %s and peer pod %s MY VETH STRUCT: %+v", localPod.Name, peerPod.Name, spew.Sdump(myVeth))
							log.Infof("local pod %s and peer pod %s PEER STRUCT: %+v", localPod.Name, peerPod.Name, spew.Sdump(peerVeth))
							if strings.Contains(err.Error(), "file exists") {
								log.Infof("race condition hit local pod %s and peer pod %s", localPod.Name, peerPod.Name)
							} else {
							    return err
							}
						}
					} else { // peerPod has higherPrio and hasn't skipped us
						// In this case we do nothing, since the pod with a higher IP is supposed to connect veth pair
						log.Infof("Doing nothing, expecting peer pod %s to connect veth pair with local pod %s", peerPod.Name, localPod.Name)
						continue
					}
				}
				if err := wireutil.SetTxChecksumOff(myVeth.LinkName, myVeth.NsName); err != nil {
					log.Errorf("Error in setting tx checksum-off on interface %s, ns %s, pod %s: %v", myVeth.LinkName, myVeth.NsName, localPod.Name, err)
					// not returning
				}
				if err := wireutil.SetTxChecksumOff(peerVeth.LinkName, peerVeth.NsName); err != nil {
					log.Errorf("Error in setting tx checksum-off on interface %s, ns %s, pod %s: %v", peerVeth.LinkName, peerVeth.NsName, peerPod.Name, err)
					// not returning
				}

			} else { // This means we're on different hosts
				log.Infof("Add: %s@%s and %s@%s are on different hosts", localPod.Name, localPod.SrcIp, peerPod.Name, peerPod.SrcIp)
				if interNodeLinkType == INTER_NODE_LINK_GRPC {
					err = CreatGRPCChan(link, localPod, peerPod, meshnetClient, &cniArgs, ctx)
					if err != nil {
						log.Errorf("Add: !! Failed to create grpc wire. err: %v", err)
						return err
					}
					continue
				}
				// Creating koko's Vxlan struct
				vxlan := makeVxlan(srcIntf, peerPod.SrcIp, link.Uid)
				// Checking if interface already exists
				iExist, _ := koko.IsExistLinkInNS(myVeth.NsName, myVeth.LinkName)
				if iExist { // If VXLAN intf exists, we need to remove it first
					log.Infof("VXLAN intf already exists, removing it first")
					if err := myVeth.RemoveVethLink(); err != nil {
						log.Infof("Failed to remove a local stale VXLAN interface %s for pod %s", myVeth.LinkName, localPod.Name)
						return err
					}
				}
				if err = koko.MakeVxLan(*myVeth, *vxlan); err != nil {
					log.Infof("Error when creating a Vxlan interface with koko: %s", err)
					return err
				}

				// Now we need to make an API call to update the remote VTEP to point to us
				payload := &mpb.RemotePod{
					NetNs:    peerPod.NetNs,
					IntfName: link.PeerIntf,
					IntfIp:   link.PeerIp,
					PeerVtep: localPod.SrcIp,
					Vni:      link.Uid + vxlanBase,
					KubeNs:   string(cniArgs.K8S_POD_NAMESPACE),
					NodeIntf: srcIntf,
				}

				url := fmt.Sprintf("%s:%s", peerPod.SrcIp, defaultPort)
				log.Infof("Trying to do a remote update on %s", url)

				remote, err := grpc.Dial(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
				if err != nil {
					log.Infof("Failed to dial remote gRPC url %s", url)
					return err
				}
				remoteClient := mpb.NewRemoteClient(remote)
				ok, err := remoteClient.Update(ctx, payload)
				if err != nil || !ok.Response {
					log.Infof("Failed to do a remote update")
					return err
				}
				log.Infof("Successfully updated remote meshnet daemon")
			}
		} else { // This means that our peer pod hasn't come up yet
			// Since there's no way of telling if our peer is going to be on this host or another,
			// the only option is to do nothing, assuming that the peer POD will do all the plumbing when it comes up
			log.Infof("Add: Peer pod %s isn't alive yet, continuing", peerPod.Name)
			// Here we need to set the skipped flag so that our peer can configure VEth interface when it comes up later
			ok, err := meshnetClient.Skip(ctx, &mpb.SkipQuery{
				Pod:    localPod.Name,
				Peer:   peerPod.Name,
				KubeNs: string(cniArgs.K8S_POD_NAMESPACE),
			})
			if err != nil || !ok.Response {
				log.Errorf("Add: Failed to set a skipped flag on peer %s", peerPod.Name)
				return err
			}
		}
	}

	return types.PrintResult(result, n.CNIVersion)
}

// -------------------------------------------------------------------------------------------------
// Deletes interfaces from a POD
func cmdDel(args *skel.CmdArgs) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cniArgs := k8sArgs{}
	if err := types.LoadArgs(args.Args, &cniArgs); err != nil {
		return err
	}
	log.Infof("Processing DEL request: %s", cniArgs.K8S_POD_NAME)
	log.WithFields(log.Fields{
		"args": fmt.Sprintf("%+v", args),
	}).Info("DEL request arguments")
	log.Info("Parsing cni .conf file")
	n, result, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	conn, err := grpc.Dial(localDaemon, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Errorf("Failed to connect to local meshnetd on %s", localDaemon)
		return err
	}
	defer conn.Close()

	meshnetClient := mpb.NewLocalClient(conn)

	log.Infof("Del: Retrieving pod's (%s@%s) metadata from meshnet daemon", string(cniArgs.K8S_POD_NAME), string(cniArgs.K8S_POD_NAMESPACE))
	localPod, err := meshnetClient.Get(ctx, &mpb.PodQuery{
		Name:   string(cniArgs.K8S_POD_NAME), // getting deatils of the current pod.
		KubeNs: string(cniArgs.K8S_POD_NAMESPACE),
	})
	if err != nil {
		log.Infof("Del: Pod %s:%s is not in topology returning. err:%v", string(cniArgs.K8S_POD_NAMESPACE), string(cniArgs.K8S_POD_NAME), err)
		return types.PrintResult(result, n.CNIVersion)
	}
	// if the current containerID is not the topo's current containerID, exit here b/c this is a duplicated DEL call.
	if localPod.ContainerId != args.ContainerID {
		log.Infof("Del: Pod %s:%s is a duplicate; skipping", string(cniArgs.K8S_POD_NAMESPACE), string(cniArgs.K8S_POD_NAME))
		return nil
	}

	/* Tell daemon to close the grpc tunnel for this pod netns (if any) */
	log.Infof("Del: Remove the pod's grpc tunnel, if any")
	wireDef := mpb.WireDef{
		KubeNs:       string(cniArgs.K8S_POD_NAMESPACE),
		LocalPodName: string(cniArgs.K8S_POD_NAME),
	}

	removResp, err := meshnetClient.RemGRPCWire(ctx, &wireDef)
	if err != nil || !removResp.Response {
		return fmt.Errorf("del: could not remove grpc wire: %v", err)
	}

	localPodSrcIp := localPod.SrcIp
	log.Infof("Del: Topology data still exists in CRs, cleaning up it's status")
	// By setting srcIP and NetNS to "" we're marking this POD as dead
	localPod.NetNs = ""
	localPod.SrcIp = ""
	localPod.ContainerId = ""
	_, err = meshnetClient.SetAlive(ctx, localPod)
	if err != nil {
		return fmt.Errorf("del: could not set alive: %v", err)
	}

	log.Infof("Del: Iterating over each link for clean-up")
	for _, link := range localPod.Links { // Iterate over each link of the local pod
		linkType := "veth"
		// Initialising peer pod's metadata. Peer pod information is needed to determine if a link is of type GRPC or VxLAN.
		// For GRPC link type there is some addtional cleanup work nedd to be perfromed.
		log.Infof("Del: Pod %s is retrieving peer pod %s information from meshnet daemon", cniArgs.K8S_POD_NAME, link.PeerPod)
		peerPod, _ := meshnetClient.Get(ctx, &mpb.PodQuery{
			Name:   link.PeerPod, // getting peer pod detail.
			KubeNs: string(cniArgs.K8S_POD_NAMESPACE),
		})

		if peerPod.SrcIp != localPodSrcIp {
			// they are on different hosts
			if interNodeLinkType == INTER_NODE_LINK_GRPC {
				// for this link bring the grpc wire down
				err = MakeGRPCChanDown(link, localPod, peerPod, ctx)
				if err != nil {
					log.Errorf("Del: !! Failed to remove remote grpc wire. err: %v", err)
					// no return
				}
				linkType = "grpc"
			} else {
				linkType = "vxlan"
			}
		}
		// Creating koko's Veth struct for local intf
		myVeth, err := makeVeth(args.Netns, link.LocalIntf, link.LocalIp)
		if err != nil {
			log.Infof("Del: Failed to construct koko Veth struct")
			return err
		}

		log.Infof("Del: Removing link %s", link.LocalIntf)
		// API call to koko to remove local Veth link
		if err = myVeth.RemoveVethLink(); err != nil {
			// instead of failing, just log the error and move on
			log.Errorf("Del: Error removing Veth link %s (%s) on pod %s: %v", link.LocalIntf, linkType, localPod.Name, err)
		}

		// Setting reversed skipped flag so that this pod will try to connect veth pair on restart
		log.Infof("Del: Setting skip-reverse flag on peer %s", link.PeerPod)
		ok, err := meshnetClient.SkipReverse(ctx, &mpb.SkipQuery{
			Pod:    localPod.Name,
			Peer:   link.PeerPod,
			KubeNs: string(cniArgs.K8S_POD_NAMESPACE),
		})
		if err != nil || !ok.Response {
			log.Errorf("Del: Failed to set skip reversed flag on our peer %s", link.PeerPod)
			return err
		}
	}
	return nil
}

func SetInterNodeLinkType() {
	// TODO: Find a more appropriate (if any) way to figure out intended link type
	// As of today, daemon gets the intended link type from env INTER_NODE_LINK_TYPE
	// which is set by deployment file. The daemon further propagates this to plugin
	// via means of file on host (which is read below) containing the value GRPC or VXLAN
	b, err := os.ReadFile("/etc/cni/net.d/meshnet-inter-node-link-type")
	if err != nil {
		log.Warningf("Could not read iner node link type: %v", err)
		// use the default value
		return
	}

	interNodeLinkType = string(b)
}

// -------------------------------------------------------------------------------------------------
func main() {
	fp, err := os.OpenFile("/var/log/meshnet-cni.log", os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err == nil {
		log.SetOutput(fp)
	}

	SetInterNodeLinkType()
	log.Infof("INTER_NODE_LINK_TYPE: %v", interNodeLinkType)

	retCode := 0
	e := skel.PluginMainWithError(cmdAdd, cmdGet, cmdDel, version.All, "CNI plugin meshnet v0.3.0")
	if e != nil {
		log.Errorf("failed to run meshnet cni: %v", e.Print())
		retCode = 1
	}
	log.Infof("meshnet cni call successful")
	fp.Close()
	os.Exit(retCode)
}

func cmdGet(args *skel.CmdArgs) error {
	log.Infof("cmdGet called: %+v", args)
	return fmt.Errorf("not implemented")
}

//-------------------------------------------------------------------------------------------------
