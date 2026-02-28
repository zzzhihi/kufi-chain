package nodemgr

import "testing"

func TestPeerInfoFromJoinRequestPrefersPeerHost(t *testing.T) {
	req := &JoinRequest{
		OrgName:    "PeerTwo",
		MSPID:      "PeerTwoMSP",
		Domain:     "peertwo.kufichain.network",
		PeerHost:   "18.141.70.237",
		PeerPort:   7051,
		AnchorHost: "peer0.peertwo.kufichain.network",
		AnchorPort: 7051,
		MgmtAddr:   "http://18.141.70.237:9500",
	}

	peer := peerInfoFromJoinRequest(req)
	if peer.PeerAddr != "18.141.70.237:7051" {
		t.Fatalf("unexpected peer addr: %s", peer.PeerAddr)
	}
	if peer.MgmtAddr != req.MgmtAddr {
		t.Fatalf("unexpected mgmt addr: %s", peer.MgmtAddr)
	}
}

func TestPeerInfoFromJoinRequestFallsBackToMgmtHost(t *testing.T) {
	req := &JoinRequest{
		OrgName:    "PeerThree",
		MSPID:      "PeerThreeMSP",
		Domain:     "peerthree.kufichain.network",
		PeerPort:   7051,
		AnchorHost: "peer0.peerthree.kufichain.network",
		AnchorPort: 7051,
		MgmtAddr:   "http://52.74.87.173:9500",
	}

	peer := peerInfoFromJoinRequest(req)
	if peer.PeerAddr != "52.74.87.173:7051" {
		t.Fatalf("unexpected peer addr: %s", peer.PeerAddr)
	}
}
