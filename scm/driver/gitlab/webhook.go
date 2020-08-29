// Copyright 2017 Drone.IO Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gitlab

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"time"

	"github.com/jenkins-x/go-scm/scm"
)

type webhookService struct {
	client *wrapper
}

func (s *webhookService) Parse(req *http.Request, fn scm.SecretFunc) (scm.Webhook, error) {
	data, err := ioutil.ReadAll(
		io.LimitReader(req.Body, 10000000),
	)
	if err != nil {
		return nil, err
	}

	var hook scm.Webhook
	event := req.Header.Get("X-Gitlab-Event")
	switch event {
	case "Push Hook", "Tag Push Hook":
		hook, err = parsePushHook(data)
	case "Issue Hook":
		return nil, scm.UnknownWebhook{event}
	case "Merge Request Hook":
		hook, err = parsePullRequestHook(data)
	case "Note Hook":
		hook, err = parseCommentHook(data)
	default:
		return nil, scm.UnknownWebhook{event}
	}
	if err != nil {
		return nil, err
	}

	// get the gitlab shared token to verify the payload
	// authenticity. If no key is provided, no validation
	// is performed.
	token, err := fn(hook)
	if err != nil {
		return hook, err
	} else if token == "" {
		return hook, nil
	}

	if subtle.ConstantTimeCompare([]byte(req.Header.Get("X-Gitlab-Token")), []byte(token)) == 0 {
		return hook, scm.ErrSignatureInvalid
	}
	return hook, nil
}

func parsePushHook(data []byte) (scm.Webhook, error) {
	src := new(pushHook)
	err := json.Unmarshal(data, src)
	if err != nil {
		return nil, err
	}
	switch {
	case src.ObjectKind == "push" && src.Before == "0000000000000000000000000000000000000000":
		// TODO we previously considered returning a
		// branch creation hook, however, the push hook
		// returns more metadata (commit details).
		return convertPushHook(src), nil
	case src.ObjectKind == "push" && src.After == "0000000000000000000000000000000000000000":
		return converBranchHook(src), nil
	case src.ObjectKind == "tag_push" && src.Before == "0000000000000000000000000000000000000000":
		// TODO we previously considered returning a
		// tag creation hook, however, the push hook
		// returns more metadata (commit details).
		return convertPushHook(src), nil
	case src.ObjectKind == "tag_push" && src.After == "0000000000000000000000000000000000000000":
		return convertTagHook(src), nil
	default:
		return convertPushHook(src), nil
	}
}

func parsePullRequestHook(data []byte) (scm.Webhook, error) {
	src := new(pullRequestHook)
	err := json.Unmarshal(data, src)
	if err != nil {
		return nil, err
	}
	if src.ObjectAttributes.Action == "" {
		src.ObjectAttributes.Action = "open"
	}
	event := src.ObjectAttributes.Action
	switch event {
	case "open", "close", "reopen", "merge", "update":
		// no-op
	default:
		return nil, scm.UnknownWebhook{event}
	}
	switch {
	default:
		return convertPullRequestHook(src), nil
	}
}

func parseCommentHook(data []byte) (scm.Webhook, error) {
	src := new(commentHook)
	err := json.Unmarshal(data, src)
	if err != nil {
		return nil, err
	}
	if src.ObjectAttributes.Action == "" {
		src.ObjectAttributes.Action = "open"
	}
	kind := src.ObjectAttributes.NoteableType
	switch kind {
	case "MergeRequest":
		return convertMergeRequestCommentHook(src), nil
	default:
		return nil, scm.UnknownWebhook{kind}
	}
}

func convertPushHook(src *pushHook) *scm.PushHook {
	repo := *convertRepositoryHook(&src.Project)
	dst := &scm.PushHook{
		Ref:  scm.ExpandRef(src.Ref, "refs/heads/"),
		Repo: repo,
		Commit: scm.Commit{
			Sha:     src.CheckoutSha,
			Message: "", // NOTE this is set below
			Author: scm.Signature{
				Login:  src.UserUsername,
				Name:   src.UserName,
				Email:  src.UserEmail,
				Avatar: src.UserAvatar,
			},
			Committer: scm.Signature{
				Login:  src.UserUsername,
				Name:   src.UserName,
				Email:  src.UserEmail,
				Avatar: src.UserAvatar,
			},
			Link: "", // NOTE this is set below
		},
		Sender: scm.User{
			Login:  src.UserUsername,
			Name:   src.UserName,
			Email:  src.UserEmail,
			Avatar: src.UserAvatar,
		},
	}
	if len(src.Commits) > 0 {
		dst.Commit.Message = src.Commits[0].Message
		dst.Commit.Link = src.Commits[0].URL
		dst.Commits = converPushHookCommits(src)
	}
	return dst
}

func converPushHookCommits(src *pushHook) []scm.PushCommit {
	var pushCommits []scm.PushCommit
	for _, commit := range src.Commits{
		scmPushCommit := scm.PushCommit{
			ID: commit.ID,
			Message: commit.Message,
			Added: commit.Added,
			Modified: commit.Modified,
			Removed: commit.Removed,
		}
		pushCommits = append(pushCommits, scmPushCommit)
	}
	return pushCommits
}

func converBranchHook(src *pushHook) *scm.BranchHook {
	action := scm.ActionCreate
	commit := src.After
	if src.After == "0000000000000000000000000000000000000000" {
		action = scm.ActionDelete
		commit = src.Before
	}
	repo := *convertRepositoryHook(&src.Project)
	return &scm.BranchHook{
		Action: action,
		Ref: scm.Reference{
			Name: scm.TrimRef(src.Ref),
			Sha:  commit,
		},
		Repo: repo,
		Sender: scm.User{
			Login:  src.UserUsername,
			Name:   src.UserName,
			Email:  src.UserEmail,
			Avatar: src.UserAvatar,
		},
	}
}

func convertTagHook(src *pushHook) *scm.TagHook {
	action := scm.ActionCreate
	commit := src.After
	if src.After == "0000000000000000000000000000000000000000" {
		action = scm.ActionDelete
		commit = src.Before
	}
	repo := *convertRepositoryHook(&src.Project)
	return &scm.TagHook{
		Action: action,
		Ref: scm.Reference{
			Name: scm.TrimRef(src.Ref),
			Sha:  commit,
		},
		Repo: repo,
		Sender: scm.User{
			Login:  src.UserUsername,
			Name:   src.UserName,
			Email:  src.UserEmail,
			Avatar: src.UserAvatar,
		},
	}
}

func convertPullRequestHook(src *pullRequestHook) *scm.PullRequestHook {
	action := scm.ActionUpdate
	switch src.ObjectAttributes.Action {
	case "open":
		action = scm.ActionOpen
	case "close":
		action = scm.ActionClose
	case "reopen":
		action = scm.ActionReopen
	case "merge":
		action = scm.ActionMerge
	case "update":
		action = scm.ActionUpdate
	}
	fork := scm.Join(
		src.ObjectAttributes.Source.Namespace,
		src.ObjectAttributes.Source.Name,
	)
	repo := *convertRepositoryHook(&src.Project)
	ref := fmt.Sprintf("refs/merge-requests/%d/head", src.ObjectAttributes.Iid)
	sha := src.ObjectAttributes.LastCommit.ID
	pr := scm.PullRequest{
		Number: src.ObjectAttributes.Iid,
		Title:  src.ObjectAttributes.Title,
		Body:   src.ObjectAttributes.Description,
		State:  gitlabStateToSCMState(src.ObjectAttributes.State),
		Sha:    sha,
		Ref:    ref,
		Base: scm.PullRequestBranch{
			Ref: repo.Branch,
		},
		Head: scm.PullRequestBranch{
			Sha: sha,
		},
		Source: src.ObjectAttributes.SourceBranch,
		Target: src.ObjectAttributes.TargetBranch,
		Fork:   fork,
		Link:   src.ObjectAttributes.URL,
		Closed: src.ObjectAttributes.State != "opened",
		Merged: src.ObjectAttributes.State == "merged",
		// Created   : src.ObjectAttributes.CreatedAt,
		// Updated  : src.ObjectAttributes.UpdatedAt, // 2017-12-10 17:01:11 UTC
		Author: scm.User{
			Login:  src.User.Username,
			Name:   src.User.Name,
			Email:  "", // TODO how do we get the pull request author email?
			Avatar: src.User.AvatarURL,
		},
	}
	pr.Base.Repo = *convertRepositoryHook(src.ObjectAttributes.Target)
	pr.Head.Repo = *convertRepositoryHook(src.ObjectAttributes.Source)
	return &scm.PullRequestHook{
		Action:      action,
		PullRequest: pr,
		Repo:        repo,
		Sender: scm.User{
			Login:  src.User.Username,
			Name:   src.User.Name,
			Email:  "", // TODO how do we get the pull request author email?
			Avatar: src.User.AvatarURL,
		},
	}
}

func convertMergeRequestCommentHook(src *commentHook) *scm.PullRequestCommentHook {
	user := scm.User{
		ID:     src.ObjectAttributes.AuthorID,
		Login:  src.User.Username,
		Name:   src.User.Name,
		Email:  "", // TODO how do we get the pull request author email?
		Avatar: src.User.AvatarURL,
	}

	fork := scm.Join(
		src.MergeRequest.Source.Namespace,
		src.MergeRequest.Source.Name,
	)

	repo := *convertRepositoryHook(&src.Project)

	ref := fmt.Sprintf("refs/merge-requests/%d/head", src.MergeRequest.Iid)
	sha := src.MergeRequest.LastCommit.ID

	// Mon Jan 2 15:04:05 -0700 MST 2006
	created_pr_at, _ := time.Parse("2006-01-02 15:04:05 MST", src.MergeRequest.CreatedAt)
	updated_pr_at, _ := time.Parse("2006-01-02 15:04:05 MST", src.MergeRequest.UpdatedAt)
	pr := scm.PullRequest{
		Number: src.MergeRequest.Iid,
		Title:  src.MergeRequest.Title,
		Body:   src.MergeRequest.Description,
		State:  gitlabStateToSCMState(src.MergeRequest.State),
		Sha:    sha,
		Ref:    ref,
		Base: scm.PullRequestBranch{
			Ref: src.MergeRequest.TargetBranch,
		},
		Head: scm.PullRequestBranch{
			Ref: src.MergeRequest.SourceBranch,
			Sha: sha,
		},
		Source:  src.MergeRequest.SourceBranch,
		Target:  src.MergeRequest.TargetBranch,
		Fork:    fork,
		Link:    src.MergeRequest.URL,
		Closed:  src.MergeRequest.State != "opened",
		Merged:  src.MergeRequest.State == "merged",
		Created: created_pr_at,
		Updated: updated_pr_at, // 2017-12-10 17:01:11 UTC
		Author:  user,
	}
	pr.Base.Repo = *convertRepositoryHook(src.MergeRequest.Target)
	pr.Head.Repo = *convertRepositoryHook(src.MergeRequest.Source)

	created_at, _ := time.Parse("2006-01-02 15:04:05 MST", src.ObjectAttributes.CreatedAt)
	updated_at, _ := time.Parse("2006-01-02 15:04:05 MST", src.ObjectAttributes.UpdatedAt)

	return &scm.PullRequestCommentHook{
		Action:      scm.ActionCreate,
		Repo:        repo,
		PullRequest: pr,
		Comment: scm.Comment{
			ID:      src.ObjectAttributes.ID,
			Body:    src.ObjectAttributes.Note,
			Author:  user, // TODO: is the user the author id ??
			Created: created_at,
			Updated: updated_at,
		},
		Sender: user,
	}
}

func convertRepositoryHook(from *project) *scm.Repository {
	namespace, name := scm.Split(from.PathWithNamespace)
	return &scm.Repository{
		ID:        strconv.Itoa(from.ID),
		Namespace: namespace,
		Name:      name,
		FullName:  from.PathWithNamespace,
		Clone:     from.GitHTTPURL,
		CloneSSH:  from.GitSSHURL,
		Link:      from.WebURL,
		Branch:    from.DefaultBranch,
		Private:   false, // TODO how do we correctly set Private vs Public?
	}
}

type (
	project struct {
		ID                int         `json:"id"`
		Name              string      `json:"name"`
		Description       string      `json:"description"`
		WebURL            string      `json:"web_url"`
		AvatarURL         interface{} `json:"avatar_url"`
		GitSSHURL         string      `json:"git_ssh_url"`
		GitHTTPURL        string      `json:"git_http_url"`
		Namespace         string      `json:"namespace"`
		VisibilityLevel   int         `json:"visibility_level"`
		PathWithNamespace string      `json:"path_with_namespace"`
		DefaultBranch     string      `json:"default_branch"`
		CiConfigPath      interface{} `json:"ci_config_path"`
		Homepage          string      `json:"homepage"`
		URL               string      `json:"url"`
		SSHURL            string      `json:"ssh_url"`
		HTTPURL           string      `json:"http_url"`
	}

	pushHook struct {
		ObjectKind   string      `json:"object_kind"`
		EventName    string      `json:"event_name"`
		Before       string      `json:"before"`
		After        string      `json:"after"`
		Ref          string      `json:"ref"`
		CheckoutSha  string      `json:"checkout_sha"`
		Message      interface{} `json:"message"`
		UserID       int         `json:"user_id"`
		UserName     string      `json:"user_name"`
		UserUsername string      `json:"user_username"`
		UserEmail    string      `json:"user_email"`
		UserAvatar   string      `json:"user_avatar"`
		ProjectID    int         `json:"project_id"`
		Project      project     `json:"project"`
		Commits      []struct {
			ID        string `json:"id"`
			Message   string `json:"message"`
			Timestamp string `json:"timestamp"`
			URL       string `json:"url"`
			Author    struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"author"`
			Added    []string      `json:"added"`
			Modified []string `json:"modified"`
			Removed  []string `json:"removed"`
		} `json:"commits"`
		TotalCommitsCount int `json:"total_commits_count"`
		Repository        struct {
			Name            string `json:"name"`
			URL             string `json:"url"`
			Description     string `json:"description"`
			Homepage        string `json:"homepage"`
			GitHTTPURL      string `json:"git_http_url"`
			GitSSHURL       string `json:"git_ssh_url"`
			VisibilityLevel int    `json:"visibility_level"`
		} `json:"repository"`
	}

	commentHook struct {
		ObjectKind string `json:"object_kind"`
		User       struct {
			Name      string `json:"name"`
			Username  string `json:"username"`
			AvatarURL string `json:"avatar_url"`
		} `json:"user"`
		ProjectID        int     `json:"project_id"`
		Project          project `json:"project"`
		ObjectAttributes struct {
			ID           int         `json:"id"`
			Note         string      `json:"note"`
			NoteableType string      `json:"noteable_type"`
			AuthorID     int         `json:"author_id"`
			CreatedAt    string      `json:"created_at"`
			UpdatedAt    string      `json:"updated_at"`
			ProjectID    int         `json:"project_id"`
			Attachment   interface{} `json:"attachment"`
			LineCode     string      `json:"line_code"`
			CommitID     string      `json:"commit_id"`
			NoteableID   int         `json:"noteable_id"`
			StDiff       interface{} `json:"st_diff"`
			System       bool        `json:"system"`
			UpdatedByID  interface{} `json:"updated_by_id"`
			Type         string      `json:"type"`
			Position     struct {
				BaseSha      string      `json:"base_sha"`
				StartSha     string      `json:"start_sha"`
				HeadSha      string      `json:"head_sha"`
				OldPath      string      `json:"old_path"`
				NewPath      string      `json:"new_path"`
				PositionType string      `json:"position_type"`
				OldLine      interface{} `json:"old_line"`
				NewLine      int         `json:"new_line"`
			} `json:"position"`
			OriginalPosition struct {
				BaseSha      string      `json:"base_sha"`
				StartSha     string      `json:"start_sha"`
				HeadSha      string      `json:"head_sha"`
				OldPath      string      `json:"old_path"`
				NewPath      string      `json:"new_path"`
				PositionType string      `json:"position_type"`
				OldLine      interface{} `json:"old_line"`
				NewLine      int         `json:"new_line"`
			} `json:"original_position"`
			ResolvedAt     interface{} `json:"resolved_at"`
			ResolvedByID   interface{} `json:"resolved_by_id"`
			DiscussionID   string      `json:"discussion_id"`
			ChangePosition struct {
				BaseSha      interface{} `json:"base_sha"`
				StartSha     interface{} `json:"start_sha"`
				HeadSha      interface{} `json:"head_sha"`
				OldPath      interface{} `json:"old_path"`
				NewPath      interface{} `json:"new_path"`
				PositionType string      `json:"position_type"`
				OldLine      interface{} `json:"old_line"`
				NewLine      interface{} `json:"new_line"`
			} `json:"change_position"`
			ResolvedByPush interface{} `json:"resolved_by_push"`
			URL            string      `json:"url"`
			Action         string      `json:"action"`
		} `json:"object_attributes"`
		Repository struct {
			Name        string `json:"name"`
			URL         string `json:"url"`
			Description string `json:"description"`
			Homepage    string `json:"homepage"`
		} `json:"repository"`
		MergeRequest struct {
			AssigneeID     interface{} `json:"assignee_id"`
			AuthorID       int         `json:"author_id"`
			CreatedAt      string      `json:"created_at"`
			DeletedAt      interface{} `json:"deleted_at"`
			Description    string      `json:"description"`
			HeadPipelineID interface{} `json:"head_pipeline_id"`
			ID             int         `json:"id"`
			Iid            int         `json:"iid"`
			LastEditedAt   interface{} `json:"last_edited_at"`
			LastEditedByID interface{} `json:"last_edited_by_id"`
			MergeCommitSha interface{} `json:"merge_commit_sha"`
			MergeError     interface{} `json:"merge_error"`
			MergeParams    struct {
				ForceRemoveSourceBranch string `json:"force_remove_source_branch"`
			} `json:"merge_params"`
			MergeStatus               string      `json:"merge_status"`
			MergeUserID               interface{} `json:"merge_user_id"`
			MergeWhenPipelineSucceeds bool        `json:"merge_when_pipeline_succeeds"`
			MilestoneID               interface{} `json:"milestone_id"`
			SourceBranch              string      `json:"source_branch"`
			SourceProjectID           int         `json:"source_project_id"`
			State                     string      `json:"state"`
			TargetBranch              string      `json:"target_branch"`
			TargetProjectID           int         `json:"target_project_id"`
			TimeEstimate              int         `json:"time_estimate"`
			Title                     string      `json:"title"`
			UpdatedAt                 string      `json:"updated_at"`
			UpdatedByID               interface{} `json:"updated_by_id"`
			URL                       string      `json:"url"`
			Source                    *project    `json:"source"`
			Target                    *project    `json:"target"`
			LastCommit                struct {
				ID        string `json:"id"`
				Message   string `json:"message"`
				Timestamp string `json:"timestamp"`
				URL       string `json:"url"`
				Author    struct {
					Name  string `json:"name"`
					Email string `json:"email"`
				} `json:"author"`
			} `json:"last_commit"`
			WorkInProgress      bool        `json:"work_in_progress"`
			TotalTimeSpent      int         `json:"total_time_spent"`
			HumanTotalTimeSpent interface{} `json:"human_total_time_spent"`
			HumanTimeEstimate   interface{} `json:"human_time_estimate"`
		} `json:"merge_request"`
	}

	tagHook struct {
		ObjectKind   string      `json:"object_kind"`
		EventName    string      `json:"event_name"`
		Before       string      `json:"before"`
		After        string      `json:"after"`
		Ref          string      `json:"ref"`
		CheckoutSha  string      `json:"checkout_sha"`
		Message      interface{} `json:"message"`
		UserID       int         `json:"user_id"`
		UserName     string      `json:"user_name"`
		UserUsername string      `json:"user_username"`
		UserEmail    string      `json:"user_email"`
		UserAvatar   string      `json:"user_avatar"`
		ProjectID    int         `json:"project_id"`
		Project      project     `json:"project"`
		Commits      []struct {
			ID        string `json:"id"`
			Message   string `json:"message"`
			Timestamp string `json:"timestamp"`
			URL       string `json:"url"`
			Author    struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"author"`
			Added    []string      `json:"added"`
			Modified []interface{} `json:"modified"`
			Removed  []interface{} `json:"removed"`
		} `json:"commits"`
		TotalCommitsCount int `json:"total_commits_count"`
		Repository        struct {
			Name            string `json:"name"`
			URL             string `json:"url"`
			Description     string `json:"description"`
			Homepage        string `json:"homepage"`
			GitHTTPURL      string `json:"git_http_url"`
			GitSSHURL       string `json:"git_ssh_url"`
			VisibilityLevel int    `json:"visibility_level"`
		} `json:"repository"`
	}

	issueHook struct {
		ObjectKind string `json:"object_kind"`
		User       struct {
			Name      string `json:"name"`
			Username  string `json:"username"`
			AvatarURL string `json:"avatar_url"`
		} `json:"user"`
		Project          project `json:"project"`
		ObjectAttributes struct {
			AssigneeID          interface{}   `json:"assignee_id"`
			AuthorID            int           `json:"author_id"`
			BranchName          interface{}   `json:"branch_name"`
			ClosedAt            interface{}   `json:"closed_at"`
			Confidential        bool          `json:"confidential"`
			CreatedAt           string        `json:"created_at"`
			DeletedAt           interface{}   `json:"deleted_at"`
			Description         string        `json:"description"`
			DueDate             interface{}   `json:"due_date"`
			ID                  int           `json:"id"`
			Iid                 int           `json:"iid"`
			LastEditedAt        string        `json:"last_edited_at"`
			LastEditedByID      int           `json:"last_edited_by_id"`
			MilestoneID         interface{}   `json:"milestone_id"`
			MovedToID           interface{}   `json:"moved_to_id"`
			ProjectID           int           `json:"project_id"`
			RelativePosition    int           `json:"relative_position"`
			State               string        `json:"state"`
			TimeEstimate        int           `json:"time_estimate"`
			Title               string        `json:"title"`
			UpdatedAt           string        `json:"updated_at"`
			UpdatedByID         int           `json:"updated_by_id"`
			URL                 string        `json:"url"`
			TotalTimeSpent      int           `json:"total_time_spent"`
			HumanTotalTimeSpent interface{}   `json:"human_total_time_spent"`
			HumanTimeEstimate   interface{}   `json:"human_time_estimate"`
			AssigneeIds         []interface{} `json:"assignee_ids"`
			Action              string        `json:"action"`
		} `json:"object_attributes"`
		Labels []struct {
			ID          int         `json:"id"`
			Title       string      `json:"title"`
			Color       string      `json:"color"`
			ProjectID   int         `json:"project_id"`
			CreatedAt   string      `json:"created_at"`
			UpdatedAt   string      `json:"updated_at"`
			Template    bool        `json:"template"`
			Description string      `json:"description"`
			Type        string      `json:"type"`
			GroupID     interface{} `json:"group_id"`
		} `json:"labels"`
		Changes struct {
			Labels struct {
				Previous []interface{} `json:"previous"`
				Current  []struct {
					ID          int         `json:"id"`
					Title       string      `json:"title"`
					Color       string      `json:"color"`
					ProjectID   int         `json:"project_id"`
					CreatedAt   string      `json:"created_at"`
					UpdatedAt   string      `json:"updated_at"`
					Template    bool        `json:"template"`
					Description string      `json:"description"`
					Type        string      `json:"type"`
					GroupID     interface{} `json:"group_id"`
				} `json:"current"`
			} `json:"labels"`
		} `json:"changes"`
		Repository struct {
			Name        string `json:"name"`
			URL         string `json:"url"`
			Description string `json:"description"`
			Homepage    string `json:"homepage"`
		} `json:"repository"`
	}

	pullRequestHook struct {
		ObjectKind string `json:"object_kind"`
		User       struct {
			Name      string `json:"name"`
			Username  string `json:"username"`
			AvatarURL string `json:"avatar_url"`
		} `json:"user"`
		Project          project `json:"project"`
		ObjectAttributes struct {
			AssigneeID     interface{} `json:"assignee_id"`
			AuthorID       int         `json:"author_id"`
			CreatedAt      string      `json:"created_at"`
			DeletedAt      interface{} `json:"deleted_at"`
			Description    string      `json:"description"`
			HeadPipelineID interface{} `json:"head_pipeline_id"`
			ID             int         `json:"id"`
			Iid            int         `json:"iid"`
			LastEditedAt   interface{} `json:"last_edited_at"`
			LastEditedByID interface{} `json:"last_edited_by_id"`
			MergeCommitSha interface{} `json:"merge_commit_sha"`
			MergeError     interface{} `json:"merge_error"`
			MergeParams    struct {
				ForceRemoveSourceBranch string `json:"force_remove_source_branch"`
			} `json:"merge_params"`
			MergeStatus               string      `json:"merge_status"`
			MergeUserID               interface{} `json:"merge_user_id"`
			MergeWhenPipelineSucceeds bool        `json:"merge_when_pipeline_succeeds"`
			MilestoneID               interface{} `json:"milestone_id"`
			SourceBranch              string      `json:"source_branch"`
			SourceProjectID           int         `json:"source_project_id"`
			State                     string      `json:"state"`
			TargetBranch              string      `json:"target_branch"`
			TargetProjectID           int         `json:"target_project_id"`
			TimeEstimate              int         `json:"time_estimate"`
			Title                     string      `json:"title"`
			UpdatedAt                 string      `json:"updated_at"`
			UpdatedByID               interface{} `json:"updated_by_id"`
			URL                       string      `json:"url"`
			Source                    *project    `json:"source"`
			Target                    *project    `json:"target"`
			LastCommit                struct {
				ID        string `json:"id"`
				Message   string `json:"message"`
				Timestamp string `json:"timestamp"`
				URL       string `json:"url"`
				Author    struct {
					Name  string `json:"name"`
					Email string `json:"email"`
				} `json:"author"`
			} `json:"last_commit"`
			WorkInProgress      bool        `json:"work_in_progress"`
			TotalTimeSpent      int         `json:"total_time_spent"`
			HumanTotalTimeSpent interface{} `json:"human_total_time_spent"`
			HumanTimeEstimate   interface{} `json:"human_time_estimate"`
			Action              string      `json:"action"`
		} `json:"object_attributes"`
		Labels  []interface{} `json:"labels"`
		Changes struct {
		} `json:"changes"`
		Repository struct {
			Name        string `json:"name"`
			URL         string `json:"url"`
			Description string `json:"description"`
			Homepage    string `json:"homepage"`
		} `json:"repository"`
	}
)
