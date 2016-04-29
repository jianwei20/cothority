package skipchain

import (
	"crypto/rand"
	"errors"

	"fmt"

	"bytes"

	"github.com/dedis/cothority/lib/cosi"
	"github.com/dedis/cothority/lib/dbg"
	"github.com/dedis/cothority/lib/network"
	"github.com/dedis/cothority/lib/sda"
)

const ServiceName = "Skipchain"

func init() {
	sda.RegisterNewService("Skipchain", newSkipchainService)
	skipchainSID = sda.ServiceFactory.ServiceID(ServiceName)
}

var skipchainSID sda.ServiceID

// Service handles adding new SkipBlocks
type Service struct {
	*sda.ServiceProcessor
	// SkipBlocks points from SkipBlockID to SkipBlock but SkipBlockID is not a valid
	// key-type for maps, so we need to cast it to string
	SkipBlocks map[string]*SkipBlock
	path       string
}

// ProposeSkipBlock takes a hash for the latest valid SkipBlock and a SkipBlock
// that will be verified. If the verification returns true, the new SkipBlock
// will be signed and added to the chain and returned.
// If the given nil as the latest block it verify if we are actually creating
// the first (genesis) block and create it. If it is called with nil although
// there already exist previous blocks, it will return an error.
func (s *Service) proposeSkipBlock(latest SkipBlockID, proposed *SkipBlock) (*ProposedSkipBlockReply, error) {
	if latest == nil || len(latest) == 0 { // genesis block creation
		// TODO set real verifier
		proposed.VerifierId = VerifyNone
		s.updateNewSkipBlock(nil, proposed)
		err := s.startPropagation(proposed)
		reply := &ProposedSkipBlockReply{
			Previous: nil, // genesis block
			Latest:   proposed,
		}
		dbg.Lvlf3("Successfuly created genesis: %+v", reply)
		return reply, err
	}

	var prev *SkipBlock
	if !latest.IsNull() {
		var ok bool
		prev, ok = s.SkipBlocks[string(latest)]
		if !ok {
			return nil, errors.New("Couldn't find latest block.")
		}
		proposed.VerifierId = prev.VerifierId
		if s.verifyNewSkipBlock(prev, proposed) {
			s.updateNewSkipBlock(prev, proposed)
		} else {
			return nil, errors.New("Verification error")
		}
	}
	reply := &ProposedSkipBlockReply{
		Previous: prev,
		Latest:   proposed,
	}

	// notify all other services with the same ID:
	if err := s.startPropagation(proposed); err != nil {
		return nil, err
	}
	return reply, nil
}

func (s *Service) ProposeSkipBlock(e *network.Entity, psbd *ProposeSkipBlock) (network.ProtocolMessage, error) {
	prop := psbd.Proposed
	if !psbd.Latest.IsNull() {
		latest, ok := s.getSkipBlockByID(psbd.Latest)
		if !ok {
			return nil, errors.New("Didn't find latest block")
		}
		prop.MaximumHeight = latest.MaximumHeight
		prop.ParentBlock = latest.ParentBlock
		prop.VerifierId = latest.VerifierId
	}
	psbr, err := s.proposeSkipBlock(psbd.Latest, psbd.Proposed)
	if err != nil {
		return nil, err
	}
	reply := &ProposedSkipBlockReply{
		Previous: psbr.Previous,
		Latest:   psbr.Latest,
	}
	return reply, nil
}

func (s *Service) updateNewSkipBlock(prev, proposed *SkipBlock) {
	dbg.Lvl4(fmt.Sprintf("\nprev=%+v\nproposed=%+v", prev, proposed))
	// later we will support higher blocks
	proposed.Height = 1

	var curID string
	proposed.BackLink = make([]SkipBlockID, proposed.Height)
	if prev == nil { // genesis
		proposed.Index++
		// genesis block has a random back-link:
		bl := make([]byte, 32)
		_, _ = rand.Read(bl)
		proposed.BackLink[0] = bl
		// empty forward link:
		proposed.Hash = proposed.calculateHash()
		curID = string(proposed.Hash)
	} else {
		proposed.Index = prev.Index + 1
		// TODO: add higher backlinks
		proposed.BackLink[0] = prev.Hash
		// update forward link of previous block:
		proposed.Hash = proposed.calculateHash()

		prev.ForwardLink = make([]*BlockLink, 1) // TODO later with height
		prev.ForwardLink[0] = NewBlockLink()
		prev.ForwardLink[0].Hash = proposed.Hash

		curID = string(proposed.Hash)
	}
	// update
	s.SkipBlocks[curID] = proposed
}

// GetUpdateChain returns a slice of SkipBlocks which describe the part of the
// skipchain from the latest block the caller knows of to the actual latest
// SkipBlock.
// Somehow comparable to search in SkipLists.
func (s *Service) GetUpdateChain(e *network.Entity, latestKnown *GetUpdateChain) (network.ProtocolMessage, error) {
	block, ok := s.getSkipBlockByID(latestKnown.Latest)
	if !ok {
		return nil, errors.New("Couldn't find latest skipblock")
	}
	// at least the latest know and the next block:
	blocks := []*SkipBlock{block}
	for len(block.ForwardLink) > 0 {
		// TODO: get highest forwardlink
		link := block.ForwardLink[0]
		block, ok = s.getSkipBlockByID(link.Hash)
		if !ok {
			return nil, errors.New("Missing block in forward-chain")
		}
		blocks = append(blocks, block)
	}
	reply := &GetUpdateChainReply{blocks}

	return reply, nil
}

// SetChildrenSkipBlock creates a new SkipChain if that 'service' doesn't exist
// yet.
func (s *Service) SetChildrenSkipBlock(e *network.Entity, scsb *SetChildrenSkipBlock) (network.ProtocolMessage, error) {
	parentID := scsb.Parent
	childID := scsb.Child
	parent, ok := s.getSkipBlockByID(parentID)
	if !ok {
		return nil, errors.New("Couldn't find skipblock!")
	}
	child, ok := s.getSkipBlockByID(childID)
	if !ok {
		return nil, errors.New("Couldn't find skipblock!")
	}
	child.ParentBlock = parentID
	parent.ChildSL = NewBlockLink()
	parent.ChildSL.Hash = childID

	err := s.startPropagation(child)
	if err != nil {
		return nil, err
	}
	// Parent-block is always of type roster, but child-block can be
	// data or roster.
	reply := &SetChildrenSkipBlockReply{parent, child}

	return reply, s.startPropagation(parent)
}

func (s *Service) getSkipBlockByID(sbID SkipBlockID) (*SkipBlock, bool) {
	b, ok := s.SkipBlocks[string(sbID)]
	return b, ok
}

// GetChildrenSkipList creates a new SkipChain if that 'service' doesn't exist
// yet.
func (s *Service) GetChildrenSkipList(sb *SkipBlock, verifier VerifierID) (*GetUpdateChainReply, error) {
	return nil, nil
}

// PropagateSkipBlockData is called when a new SkipBlock or updated SkipBlock is
// available.
func (s *Service) PropagateSkipBlock(e *network.Entity, latest *PropagateSkipBlock) (network.ProtocolMessage, error) {
	s.SkipBlocks[string(latest.SkipBlock.Hash)] = latest.SkipBlock
	return nil, nil
}

// notify other services about new/updated skipblock
func (s *Service) startPropagation(latest *SkipBlock) error {
	for _, e := range latest.EntityList.List {
		var cr *sda.ServiceMessage
		var err error
		cr, err = sda.CreateServiceMessage(ServiceName,
			&PropagateSkipBlock{latest})
		if err != nil {
			return err
		}
		if err := s.SendRaw(e, cr); err != nil {
			return err
		}
	}
	return nil
}

// SignBlock signs off the new block pointed to by the hash by first
// verifying its validity and then collectively signing off the block.
// The new signature is NOT broadcasted to the roster!
func (s *Service) SignBlock(sb *SkipBlock) error {
	prev, ok := s.SkipBlocks[string(sb.BackLink[0])]
	if !ok {
		return errors.New("Didn't find SkipBlock")
	}
	if !s.verifyNewSkipBlock(prev, sb) {
		return errors.New("Refused")
	}
	// TODO: sign off the block with the roster
	sb.Signature = cosi.NewSignature(network.Suite)
	return nil
}

// ForwardSignature asks this responsible for a SkipChain to sign off
// a new ForwardLink. Upon success the new signature will be
// broadcast to the entire roster and all backward- and forward-links.
// It returns the SkipBlock with the updated ForwardSignature or an error.
func (s *Service) ForwardSignature(updating *ForwardSignature) (*SkipBlock, error) {
	current, ok := s.SkipBlocks[string(updating.ToUpdate)]
	if !ok {
		return nil, errors.New("Didn't find SkipBlock")
	}
	if updating.Latest.VerifySignatures() != nil {
		return nil, errors.New("Couldn't verify signature of new block")
	}
	latest := updating.Latest
	updateHeight := 0
	latestHeight := len(latest.BackLink)
	for updateHeight = 0; updateHeight < latestHeight; updateHeight++ {
		if bytes.Equal(latest.BackLink[updateHeight], current.Hash) {
			break
		}
	}
	if updateHeight == latestHeight {
		return nil, errors.New("Didn't find ourselves in the backlinks")
	}
	currHeight := len(current.ForwardLink)
	if currHeight == 0 {
		current.ForwardLink = make([]*BlockLink, 0, current.Height)
		// As we are the direct predecessor of the block, we need
		// to verify using the verification-function whether that
		// block is valid or not.
		if !s.verifyNewSkipBlock(current, updating.Latest) {
			return nil, errors.New("New SkipBlock not accepted!")
		}
	} else {
		// We only need to verify that we have a complete link-history
		// from ourselves to the proposed SkipBlock
		if !s.verifyLinkedSkipBlock(current, updating.Latest) {
			return nil, errors.New("Didn't find a valid update-path")
		}
	}
	current.ForwardLink[currHeight].Hash = updating.Latest.Hash

	// TODO: sign off on the forward-link (signature on hash of current and
	// following block)
	return current, nil
}

// NewProtocol is called on all nodes of a Tree (except the root, since it is
// the one starting the protocol) so it's the Service that will be called to
// generate the PI on all others node.
func (s *Service) NewProtocol(tn *sda.TreeNodeInstance, conf *sda.GenericConfig) (sda.ProtocolInstance, error) {
	dbg.Lvl1("SkipChain received New Protocol event", tn, conf)
	return nil, nil
}

// verifyNewSkipBlock calls the appropriate app-verification and returns
// either a signature on the newest SkipBlock or nil if the SkipBlock
// has been refused
func (s *Service) verifyNewSkipBlock(latest, newest *SkipBlock) bool {
	// TODO: implement a couple of protocols that can check all
	// TODO: Verify* constants
	switch newest.VerifierId {
	case VerifyNone:
		return len(latest.ForwardLink) == 0
	}
	return false
}

// verifyLinkedSkipBlock checks if we have a valid link connecting the two
// SkipBlocks with each other.
func (s *Service) verifyLinkedSkipBlock(latest, newest *SkipBlock) bool {
	// TODO: check we have a valid link
	return true
}

func newSkipchainService(c sda.Context, path string) sda.Service {
	s := &Service{
		ServiceProcessor: sda.NewServiceProcessor(c),
		path:             path,
		SkipBlocks:       make(map[string]*SkipBlock),
	}
	if err := s.RegisterMessage(s.PropagateSkipBlock); err != nil {
		dbg.Fatal("Registration error:", err)
	}
	if err := s.RegisterMessage(s.ProposeSkipBlock); err != nil {
		dbg.Fatal("Registration error:", err)
	}
	if err := s.RegisterMessage(s.SetChildrenSkipBlock); err != nil {
		dbg.Fatal("Registration error:", err)
	}
	if err := s.RegisterMessage(s.GetUpdateChain); err != nil {
		dbg.Fatal("Registration error:", err)
	}
	return s
}
