package network

import (
	"context"
	"encoding/json"
	"encoding/hex"
	"fmt"
	"log"
	"math/rand"
	"sync"

	"github.com/bleasey/bdns/internal/blockchain"
	"github.com/bleasey/bdns/internal/index"
	"github.com/bleasey/bdns/internal/consensus"
	"github.com/libp2p/go-libp2p/core/network"
)

// Node represents a blockchain peer
type Node struct {
	Address         string
	Port            int
	Config          NodeConfig
	P2PNetwork      *P2PNetwork
	KeyPair         *blockchain.KeyPair
	RegistryKeys    [][]byte
	SlotLeaders     map[int64][]byte // epoch to slot leader
	SlotMutex       sync.Mutex
	TransactionPool map[int]*blockchain.Transaction
	TxMutex         sync.Mutex
	IndexManager    *index.IndexManager
	Blockchain      *blockchain.Blockchain
	BcMutex         sync.Mutex
	RandomNumber    []byte
    RandomMutex     sync.Mutex 
	EpochRandoms    map[int64]map[string]consensus.SecretValues
}

// Node Config
type NodeConfig struct {
	InitialTimestamp int64
	EpochInterval    int64
	Seed             float64
}

type RandomNumberMsg struct {
    Epoch        int64
    SecretValue  int     // u_i value
    RandomValue  int     // r_i value
    Sender       []byte  // Registry's public key
}

// NewNode initializes a blockchain node
func NewNode(ctx context.Context, addr string, topicName string) (*Node, error) {
	p2p, err := NewP2PNetwork(ctx, addr, topicName)
	if err != nil {
		return nil, err
	}

	node := &Node{
		Address:         p2p.Host.Addrs()[0].String(),
		P2PNetwork:      p2p,
		KeyPair:         blockchain.NewKeyPair(),
		SlotLeaders:     make(map[int64][]byte),
		TransactionPool: make(map[int]*blockchain.Transaction),
		IndexManager:    index.NewIndexManager(),
		Blockchain:      nil,
		EpochRandoms:    make(map[int64]map[string]consensus.SecretValues),
	}

	go node.ListenForDirectMessages()
	go node.P2PNetwork.ListenForGossip()
	go node.HandleGossip()
	return node, nil
}

func (n *Node) GenerateRandomNumber() []byte {
    n.RandomMutex.Lock()
    defer n.RandomMutex.Unlock()
    
    randomBytes := make([]byte, 32)
    if _, err := rand.Read(randomBytes); err != nil {
        log.Panic("Failed to generate random number:", err)
    }
    
    n.RandomNumber = randomBytes
    return randomBytes
}

// HandleGossip listens for messages from the gossip network
func (n *Node) HandleGossip() {
	for msg := range n.P2PNetwork.MsgChan {
		switch msg.Type {
		case DNSRequest:
			var req BDNSRequest
			err := json.Unmarshal(msg.Content, &req)
			if err != nil {
				log.Println("Failed during unmarshalling")
			}
			n.DNSRequestHandler(req, msg.Sender)

		case MsgTransaction:
			var tx blockchain.Transaction
			err := json.Unmarshal(msg.Content, &tx)
			if err != nil {
				log.Println("Failed during unmarshalling")
			}
			n.AddTransaction(&tx)

		case MsgBlock:
			var block blockchain.Block
			err := json.Unmarshal(msg.Content, &block)
			if err != nil {
				log.Println("Failed during unmarshalling")
			}
			n.AddBlock(&block)

		case MsgRandomNumber:
            var randomMsg RandomNumberMsg
            err := json.Unmarshal(msg.Content, &randomMsg)
            if err != nil {
                log.Println("Failed to unmarshal random number message:", err)
                continue
            }
            n.RandomNumberHandler(randomMsg.Epoch, hex.EncodeToString(randomMsg.Sender), randomMsg.SecretValue, randomMsg.RandomValue) // Store the received random number

			// case MsgChainRequest:
			// 	n.Blockchain.SendBlockchain(conn)

			// case MsgChainResponse:
			// 	n.Blockchain.ReplaceChain(conn, &n.BcMutex)
		}
	}

	fmt.Println("Exiting gossip listener for ", n.Address)
}

// Handles direct peer-to-peer messages
func (n *Node) ListenForDirectMessages() {
	// Handler for dns response
	n.P2PNetwork.Host.SetStreamHandler("/dns-response", func(s network.Stream) {
		defer s.Close()
		var response GossipMessage
		if err := json.NewDecoder(s).Decode(&response); err != nil {
			log.Println("Error decoding direct response:", err)
			return
		}

		if response.Type != DNSResponse {
			log.Println("Invalid message type received")
			return
		}

		var res BDNSResponse
		err := json.Unmarshal(response.Content, &res)
		if err != nil {
			log.Println("Failed during unmarshalling")
		}
		n.DNSResponseHandler(res)
	})
}

// BroadcastTransaction sends a new transaction to peers
func (n *Node) BroadcastTransaction(tx blockchain.Transaction) {
	n.AddTransaction(&tx)
	n.P2PNetwork.BroadcastMessage(MsgTransaction, tx)
}

func (n *Node) MakeDNSRequest(domainName string) {
	req := BDNSRequest{DomainName: domainName}
	n.P2PNetwork.BroadcastMessage(DNSRequest, req)
}

func (n *Node) BroadcastRandomNumber(epoch int64, registryKeys [][]byte) {
	_, secretValues := consensus.CommitmentPhase(n.RegistryKeys)
	nodeSecretValues := secretValues[hex.EncodeToString(n.KeyPair.PublicKey)]

	msg := RandomNumberMsg{
		Epoch: epoch,
		SecretValue: nodeSecretValues.SecretValue,
		RandomValue: nodeSecretValues.RandomValue,
		Sender: n.KeyPair.PublicKey,
	}
	n.RandomNumberHandler(epoch, hex.EncodeToString(n.KeyPair.PublicKey), nodeSecretValues.SecretValue, nodeSecretValues.RandomValue)
    n.P2PNetwork.BroadcastMessage(MsgRandomNumber, msg)
}

func (n *Node) DNSRequestHandler(req BDNSRequest, reqSender string) {
	n.TxMutex.Lock()
	defer n.TxMutex.Unlock()
	tx := n.IndexManager.GetIP(req.DomainName)
	if tx != nil {
		res := BDNSResponse{
			Timestamp:  tx.Timestamp,
			DomainName: tx.DomainName,
			IP:         tx.IP,
			TTL:        tx.TTL,
			OwnerKey:   tx.OwnerKey,
			Signature:  tx.Signature,
		}
		n.P2PNetwork.DirectMessage(DNSResponse, res, reqSender)
	}
	fmt.Println("DNS Request received at ", n.Address, " -> ", req.DomainName)
}

func (n *Node) DNSResponseHandler(res BDNSResponse) {
	fmt.Println("DNS Response received at ", n.Address, " -> ", res.DomainName, " IP:", res.IP)
}

func (n *Node) RandomNumberHandler(epoch int64, sender string, secretValue int, randomValue int) {
	n.RandomMutex.Lock()
	defer n.RandomMutex.Unlock()
	
	if n.EpochRandoms == nil {
		n.EpochRandoms = make(map[int64]map[string]consensus.SecretValues)
	}

	if n.EpochRandoms[epoch] == nil {
        n.EpochRandoms[epoch] = make(map[string]consensus.SecretValues)
    }

    n.EpochRandoms[epoch][sender] = consensus.SecretValues{
		SecretValue: secretValue,
		RandomValue: randomValue,
	}
}
