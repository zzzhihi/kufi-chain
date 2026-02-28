package main

import (
	"path/filepath"
	"testing"

	"github.com/fabric-payment-gateway/internal/nodemgr"
)

func TestCollectKnownPeersIncludesLocalNodeAndDeduplicates(t *testing.T) {
	baseDir := t.TempDir()
	store := nodemgr.NewStore(baseDir)
	if err := store.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}
	if err := store.AddPeer(nodemgr.PeerInfo{
		OrgName:  "PeerOne",
		MSPID:    "PeerOneMSP",
		Domain:   "peerone.kufichain.network",
		PeerAddr: "10.0.0.1:7051",
		MgmtAddr: "http://10.0.0.1:9500",
	}); err != nil {
		t.Fatalf("add peer: %v", err)
	}

	cfg := &nodemgr.NodeConfig{
		Role:         nodemgr.RolePeer,
		OrgName:      "PeerOne",
		MSPID:        "PeerOneMSP",
		Domain:       "peerone.kufichain.network",
		ExternalHost: "18.141.70.237",
		PeerPort:     7051,
		MgmtPort:     9500,
	}

	peers := collectKnownPeers(store, cfg)
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(peers))
	}
	if peers[0].PeerAddr != "18.141.70.237:7051" {
		t.Fatalf("expected local peer addr override, got %s", peers[0].PeerAddr)
	}
}

func TestBuildSignaturePolicyUsesRegistryPeers(t *testing.T) {
	baseDir := t.TempDir()
	store := nodemgr.NewStore(baseDir)
	if err := store.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}
	for _, peer := range []nodemgr.PeerInfo{
		{OrgName: "PeerThree", MSPID: "PeerThreeMSP", Domain: "peerthree.kufichain.network"},
		{OrgName: "PeerOne", MSPID: "PeerOneMSP", Domain: "peerone.kufichain.network"},
	} {
		if err := store.AddPeer(peer); err != nil {
			t.Fatalf("add peer %s: %v", peer.MSPID, err)
		}
	}

	cfg := &nodemgr.NodeConfig{
		Role:         nodemgr.RolePeer,
		OrgName:      "PeerTwo",
		MSPID:        "PeerTwoMSP",
		Domain:       "peertwo.kufichain.network",
		ExternalHost: "127.0.0.1",
		PeerPort:     8051,
		MgmtPort:     9502,
		DeployDir:    filepath.Join(baseDir, "deploy"),
	}

	policy := buildSignaturePolicy(store, cfg)
	want := "OR('PeerOneMSP.peer','PeerThreeMSP.peer','PeerTwoMSP.peer')"
	if policy != want {
		t.Fatalf("unexpected policy: %s", policy)
	}
}
