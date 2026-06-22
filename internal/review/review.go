// Package review provides code review functionality for Kai changesets.
package review

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"

	"kai/internal/graph"
	"kai/internal/util"
)

// State represents the state of a review.
type State string

const (
	StateDraft     State = "draft"
	StateOpen      State = "open"
	StateApproved  State = "approved"
	StateChanges   State = "changes_requested"
	StateMerged    State = "merged"
	StateAbandoned State = "abandoned"
)

// Review represents a code review for a changeset or workspace.
type Review struct {
	ID                      []byte
	Title                   string
	Description             string
	State                   State
	Author                  string
	Reviewers               []string
	Assignees               []string
	TargetID                []byte // ChangeSet or Workspace ID
	TargetKind              graph.NodeKind
	TargetBranch            string
	ChangesRequestedSummary string
	ChangesRequestedBy      string
	CreatedAt               int64
	UpdatedAt               int64
}

// Comment represents a review comment.
type Comment struct {
	ID        []byte
	ReviewID  []byte
	Author    string
	Body      string
	AnchorID  []byte // Symbol or File node ID (optional)
	FilePath  string // For file:line anchors
	Line      int    // For file:line anchors
	ParentID  string // For reply threading (optional)
	CreatedAt int64
}

// Manager handles review operations.
type Manager struct {
	db *graph.DB
}

// NewManager creates a new review manager.
func NewManager(db *graph.DB) *Manager {
	return &Manager{db: db}
}

// Open creates a new review for a changeset or workspace.
func (m *Manager) Open(targetID []byte, title, description, author string, reviewers, assignees []string) (*Review, error) {
	// Verify target exists and get its kind
	target, err := m.db.GetNode(targetID)
	if err != nil {
		return nil, fmt.Errorf("getting target: %w", err)
	}
	if target == nil {
		return nil, fmt.Errorf("target not found")
	}

	// Validate target kind
	if target.Kind != graph.KindChangeSet && target.Kind != graph.KindWorkspace {
		return nil, fmt.Errorf("target must be a ChangeSet or Workspace, got %s", target.Kind)
	}

	// Generate UUID-like ID for review
	reviewID := make([]byte, 16)
	if _, err := rand.Read(reviewID); err != nil {
		return nil, fmt.Errorf("generating review ID: %w", err)
	}

	now := util.NowMs()
	payload := map[string]interface{}{
		"title":       title,
		"description": description,
		"state":       string(StateDraft),
		"author":      author,
		"reviewers":   reviewers,
		"assignees":   assignees,
		"targetId":    util.BytesToHex(targetID),
		"targetKind":  string(target.Kind),
		"createdAt":   now,
		"updatedAt":   now,
	}

	tx, err := m.db.BeginTx()
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	// Insert review node
	if err := m.db.InsertReview(tx, reviewID, payload); err != nil {
		return nil, fmt.Errorf("inserting review: %w", err)
	}

	// Create REVIEW_OF edge
	if err := m.db.InsertEdge(tx, reviewID, graph.EdgeReviewOf, targetID, nil); err != nil {
		return nil, fmt.Errorf("inserting REVIEW_OF edge: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	return &Review{
		ID:          reviewID,
		Title:       title,
		Description: description,
		State:       StateDraft,
		Author:      author,
		Reviewers:   reviewers,
		Assignees:   assignees,
		TargetID:    targetID,
		TargetKind:  target.Kind,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// Get retrieves a review by ID.
func (m *Manager) Get(reviewID []byte) (*Review, error) {
	node, err := m.db.GetNode(reviewID)
	if err != nil {
		return nil, err
	}
	if node == nil {
		return nil, nil
	}
	if node.Kind != graph.KindReview {
		return nil, fmt.Errorf("not a review: %s", node.Kind)
	}

	return nodeToReview(node)
}

// GetByShortID retrieves a review by short hex prefix.
func (m *Manager) GetByShortID(prefix string) (*Review, error) {
	reviews, err := m.List()
	if err != nil {
		return nil, err
	}

	var matches []*Review
	for _, r := range reviews {
		idHex := hex.EncodeToString(r.ID)
		if len(prefix) <= len(idHex) && idHex[:len(prefix)] == prefix {
			matches = append(matches, r)
		}
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("review not found: %s", prefix)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("ambiguous review prefix: %s (matches %d reviews)", prefix, len(matches))
	}

	return matches[0], nil
}

// List returns all reviews.
func (m *Manager) List() ([]*Review, error) {
	nodes, err := m.db.GetNodesByKind(graph.KindReview)
	if err != nil {
		return nil, err
	}

	reviews := make([]*Review, 0, len(nodes))
	for _, node := range nodes {
		r, err := nodeToReview(node)
		if err != nil {
			return nil, err
		}
		reviews = append(reviews, r)
	}

	return reviews, nil
}

// validTransitions defines which state transitions are allowed.
var validTransitions = map[State][]State{
	StateDraft:    {StateOpen, StateAbandoned},
	StateOpen:     {StateApproved, StateChanges, StateMerged, StateAbandoned},
	StateApproved: {StateMerged, StateChanges, StateAbandoned},
	StateChanges:  {StateOpen, StateApproved, StateAbandoned},
	// Terminal states: no transitions from StateMerged or StateAbandoned
}

// UpdateState changes the state of a review, validating the transition.
func (m *Manager) UpdateState(reviewID []byte, newState State, actor, summary string) error {
	node, err := m.db.GetNode(reviewID)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("review not found")
	}

	currentState := State(node.Payload["state"].(string))

	allowed, ok := validTransitions[currentState]
	if !ok {
		return fmt.Errorf("review is in terminal state %q and cannot be updated", currentState)
	}

	valid := false
	for _, s := range allowed {
		if s == newState {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("cannot transition from %q to %q", currentState, newState)
	}

	node.Payload["state"] = string(newState)
	node.Payload["updatedAt"] = util.NowMs()

	if newState == StateChanges {
		if summary != "" {
			node.Payload["changesRequestedSummary"] = summary
		}
		if actor != "" {
			node.Payload["changesRequestedBy"] = actor
		}
	} else {
		delete(node.Payload, "changesRequestedSummary")
		delete(node.Payload, "changesRequestedBy")
	}

	return m.db.UpdateNodePayload(reviewID, node.Payload)
}

// Update updates review metadata (title, description, assignees).
func (m *Manager) Update(reviewID []byte, title, description *string, assignees []string) error {
	node, err := m.db.GetNode(reviewID)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("review not found")
	}

	if title != nil {
		node.Payload["title"] = *title
	}
	if description != nil {
		node.Payload["description"] = *description
	}
	if assignees != nil {
		node.Payload["assignees"] = assignees
	}
	node.Payload["updatedAt"] = util.NowMs()

	return m.db.UpdateNodePayload(reviewID, node.Payload)
}

// Close closes a review with a final state.
func (m *Manager) Close(reviewID []byte, state State) error {
	if state != StateMerged && state != StateAbandoned {
		return fmt.Errorf("close state must be 'merged' or 'abandoned'")
	}
	return m.UpdateState(reviewID, state, "", "")
}

// Approve marks a review as approved.
func (m *Manager) Approve(reviewID []byte) error {
	return m.UpdateState(reviewID, StateApproved, "", "")
}

// RequestChanges marks a review as needing changes.
func (m *Manager) RequestChanges(reviewID []byte, actor, summary string) error {
	return m.UpdateState(reviewID, StateChanges, actor, summary)
}

// MarkReady transitions a review from draft to open.
func (m *Manager) MarkReady(reviewID []byte) error {
	review, err := m.Get(reviewID)
	if err != nil {
		return err
	}
	if review == nil {
		return fmt.Errorf("review not found")
	}
	if review.State != StateDraft {
		return fmt.Errorf("review is not in draft state")
	}
	return m.UpdateState(reviewID, StateOpen, "", "")
}

// GetTarget retrieves the target (ChangeSet or Workspace) of a review.
func (m *Manager) GetTarget(reviewID []byte) (*graph.Node, error) {
	edges, err := m.db.GetEdges(reviewID, graph.EdgeReviewOf)
	if err != nil {
		return nil, err
	}
	if len(edges) == 0 {
		return nil, fmt.Errorf("review has no target")
	}

	return m.db.GetNode(edges[0].Dst)
}

// AddComment adds a comment to a review, optionally anchored to a symbol or file:line.
func (m *Manager) AddComment(reviewID []byte, author, body, parentID string, anchor *CommentAnchor) (*Comment, error) {
	// Verify review exists
	rev, err := m.Get(reviewID)
	if err != nil {
		return nil, err
	}
	if rev == nil {
		return nil, fmt.Errorf("review not found")
	}

	commentID := make([]byte, 16)
	if _, err := rand.Read(commentID); err != nil {
		return nil, fmt.Errorf("generating comment ID: %w", err)
	}

	now := util.NowMs()
	payload := map[string]interface{}{
		"author":    author,
		"body":      body,
		"reviewId":  util.BytesToHex(reviewID),
		"createdAt": now,
	}

	comment := &Comment{
		ID:        commentID,
		ReviewID:  reviewID,
		Author:    author,
		Body:      body,
		ParentID:  parentID,
		CreatedAt: now,
	}

	if parentID != "" {
		payload["parentId"] = parentID
	}

	if anchor != nil {
		payload["filePath"] = anchor.FilePath
		payload["line"] = anchor.Line
		payload["anchorId"] = util.BytesToHex(anchor.NodeID)
		comment.FilePath = anchor.FilePath
		comment.Line = anchor.Line
		comment.AnchorID = anchor.NodeID
	}

	tx, err := m.db.BeginTx()
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	if err := m.db.InsertReviewComment(tx, commentID, payload); err != nil {
		return nil, fmt.Errorf("inserting comment: %w", err)
	}

	// Link comment to review
	if err := m.db.InsertEdge(tx, reviewID, graph.EdgeHasComment, commentID, nil); err != nil {
		return nil, fmt.Errorf("inserting HAS_COMMENT edge: %w", err)
	}

	// If anchored to a node, create ANCHORS_TO edge
	if anchor != nil && len(anchor.NodeID) > 0 {
		if err := m.db.InsertEdge(tx, commentID, graph.EdgeAnchorsTo, anchor.NodeID, nil); err != nil {
			return nil, fmt.Errorf("inserting ANCHORS_TO edge: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	// Update review's updatedAt
	_ = m.touchReview(reviewID)

	return comment, nil
}

// CommentAnchor specifies where a comment is anchored.
type CommentAnchor struct {
	NodeID   []byte // Symbol or File node ID
	FilePath string // For file:line display
	Line     int    // Line number (0 = not line-anchored)
}

// ListComments returns all comments for a review, ordered by creation time.
func (m *Manager) ListComments(reviewID []byte) ([]*Comment, error) {
	edges, err := m.db.GetEdges(reviewID, graph.EdgeHasComment)
	if err != nil {
		return nil, fmt.Errorf("getting comment edges: %w", err)
	}

	comments := make([]*Comment, 0, len(edges))
	for _, edge := range edges {
		node, err := m.db.GetNode(edge.Dst)
		if err != nil || node == nil {
			continue
		}
		c := nodeToComment(node)
		comments = append(comments, c)
	}

	// Sort by creation time (oldest first)
	sort.Slice(comments, func(i, j int) bool {
		return comments[i].CreatedAt < comments[j].CreatedAt
	})

	return comments, nil
}

// touchReview updates the review's updatedAt timestamp.
func (m *Manager) touchReview(reviewID []byte) error {
	node, err := m.db.GetNode(reviewID)
	if err != nil || node == nil {
		return err
	}
	node.Payload["updatedAt"] = util.NowMs()
	return m.db.UpdateNodePayload(reviewID, node.Payload)
}

// nodeToComment converts a graph node to a Comment.
func nodeToComment(node *graph.Node) *Comment {
	author, _ := node.Payload["author"].(string)
	body, _ := node.Payload["body"].(string)
	reviewHex, _ := node.Payload["reviewId"].(string)
	filePath, _ := node.Payload["filePath"].(string)
	line, _ := node.Payload["line"].(float64)
	createdAt, _ := node.Payload["createdAt"].(float64)
	anchorHex, _ := node.Payload["anchorId"].(string)
	parentID, _ := node.Payload["parentId"].(string)

	reviewID, _ := util.HexToBytes(reviewHex)
	anchorID, _ := util.HexToBytes(anchorHex)

	return &Comment{
		ID:        node.ID,
		ReviewID:  reviewID,
		Author:    author,
		Body:      body,
		AnchorID:  anchorID,
		FilePath:  filePath,
		Line:      int(line),
		ParentID:  parentID,
		CreatedAt: int64(createdAt),
	}
}

// nodeToReview converts a graph node to a Review struct.
func nodeToReview(node *graph.Node) (*Review, error) {
	title, _ := node.Payload["title"].(string)
	description, _ := node.Payload["description"].(string)
	state, _ := node.Payload["state"].(string)
	author, _ := node.Payload["author"].(string)
	targetHex, _ := node.Payload["targetId"].(string)
	targetKind, _ := node.Payload["targetKind"].(string)
	targetBranch, _ := node.Payload["targetBranch"].(string)
	changesRequestedSummary, _ := node.Payload["changesRequestedSummary"].(string)
	changesRequestedBy, _ := node.Payload["changesRequestedBy"].(string)
	createdAt, _ := node.Payload["createdAt"].(float64)
	updatedAt, _ := node.Payload["updatedAt"].(float64)

	targetID, _ := util.HexToBytes(targetHex)

	// Parse string arrays
	parseStringArray := func(key string) []string {
		var result []string
		if arr, ok := node.Payload[key].([]interface{}); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					result = append(result, s)
				}
			}
		}
		return result
	}

	return &Review{
		ID:                      node.ID,
		Title:                   title,
		Description:             description,
		State:                   State(state),
		Author:                  author,
		Reviewers:               parseStringArray("reviewers"),
		Assignees:               parseStringArray("assignees"),
		TargetID:                targetID,
		TargetKind:              graph.NodeKind(targetKind),
		TargetBranch:            targetBranch,
		ChangesRequestedSummary: changesRequestedSummary,
		ChangesRequestedBy:      changesRequestedBy,
		CreatedAt:               int64(createdAt),
		UpdatedAt:               int64(updatedAt),
	}, nil
}

// IDToHex converts review ID to hex string.
func IDToHex(id []byte) string {
	return hex.EncodeToString(id)
}
