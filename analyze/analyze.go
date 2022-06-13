// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package analyze

import (
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/google/go-github/v45/github"
	"github.com/spencerkimball/stargazers/fetch"
)

const (
	// nMostCorrelated is the number of correlated starred or subscribed
	// repos to include in the csv output.
	nMostCorrelated = 50
)

type Stargazers []*fetch.Stargazer

func (slice Stargazers) Len() int {
	return len(slice)
}

func (slice Stargazers) Less(i, j int) bool {
	return slice[i].StarredAt < slice[j].StarredAt
}

func (slice Stargazers) Swap(i, j int) {
	slice[i], slice[j] = slice[j], slice[i]
}

type Contributors []*fetch.Stargazer

func (slice Contributors) Len() int {
	return len(slice)
}

func (slice Contributors) Less(i, j int) bool {
	iC, _, _ := slice[i].TotalCommits()
	jC, _, _ := slice[j].TotalCommits()
	return iC > jC /* descending order */
}

func (slice Contributors) Swap(i, j int) {
	slice[i], slice[j] = slice[j], slice[i]
}

type RepoCount struct {
	name  string
	count int
}

type RepoCounts []*RepoCount

func (slice RepoCounts) Len() int {
	return len(slice)
}

func (slice RepoCounts) Less(i, j int) bool {
	return slice[i].count > slice[j].count /* descending order */
}

func (slice RepoCounts) Swap(i, j int) {
	slice[i], slice[j] = slice[j], slice[i]
}

// RunAll runs all analyses.
func RunAll(c *fetch.Context, sg []*fetch.Stargazer, rs map[string]*fetch.Repo, commits []*github.RepositoryCommit, issues []*fetch.IssueComment) error {
	if err := RunCumulativeStars(c, sg); err != nil {
		return err
	}
	if err := RunCorrelatedRepos(c, "starred", sg, rs); err != nil {
		return err
	}
	if err := RunCorrelatedRepos(c, "subscribed", sg, rs); err != nil {
		return err
	}
	if err := RunFollowers(c, sg); err != nil {
		return err
	}
	if err := RunCommitters(c, sg, rs); err != nil {
		return err
	}
	if err := RunAttributesByTime(c, sg, rs); err != nil {
		return err
	}
	if err := RunCommits(c, commits); err != nil {
		return err
	}
	if err := RunIssues(c, issues); err != nil {
		return err
	}

	return nil
}

func RunCommits(c *fetch.Context, commits []*github.RepositoryCommit) error {
	log.Printf("running all committers analysis")

	// Create a map to track how many different users committed in any given
	// month. The top level map is keyed on the month, and each value is map that
	// functions as a set.
	committersByMonth := map[string](*map[string]bool){}

	for _, commit := range commits {
		date := commit.Commit.Author.Date
		monthKey := fmt.Sprintf("%d-%d", date.Year(), date.Month())

		// Check if we've previously logged a commit in this month. If we haven't,
		// create a login.
		monthMap, ok := committersByMonth[monthKey]
		if !ok {
			monthMap = &map[string]bool{}
			committersByMonth[monthKey] = monthMap
		}

		// In both maps, just blindly insert the user to the map -- even if we've
		// inserted it before, it doesn't matter since we just mark it as true.
		(*monthMap)[*commit.Author.Login] = true
	}

	now := time.Now()
	// Go's very nitpicky about these types, but these are just typecasts over
	// int. Unclear if there are any typesafety issues with this?
	endYear := int(now.Year())
	endMonth := int(now.Month())

	// TODO(vikram): Make this a paramter. Currently hardcoded to when Aqueduct
	// was released.
	year := 2022
	month := 5

	// Set up the CSV that we're going to write the results to.
	f, err := createFile(c, "cumulative_committers.csv")
	if err != nil {
		return fmt.Errorf("failed to create file: %s", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write([]string{"Month", "Total", "Cumulative"}); err != nil {
		return fmt.Errorf("failed to write to CSV: %s", err)
	}

	allCommitters := map[string]bool{}
	for year <= endYear && month <= endMonth {
		// TODO(vikram): Is it better to keep this as a string or to store it as a
		// datetime? The latter I think will make it hard for grouping, which is
		// why its currently a string.
		key := fmt.Sprintf("%d-%d", year, month)
		monthCommitters, ok := committersByMonth[key]

		// Increment the month we're on and check if we've rolled over to the next
		// year. We do this before the potential continue call below to avoid
		// infinite loops.
		month += 1
		if month > 12 {
			month = 1
			year += 1
		}

		if !ok { // There were no committers this month. Unlikely but just in case?
			continue
		}

		for committer := range *monthCommitters {
			// We can blindly add it to the map. It doesn't matter if we re-add
			// because it just sets the value in the map to `true` again.
			allCommitters[committer] = true
		}

		row := []string{key, fmt.Sprintf("%d", len(*monthCommitters)), fmt.Sprintf("%d", len(allCommitters))}
		if err := w.Write(row); err != nil {
			return fmt.Errorf("failed to write to CSV: %s", err)
		}
	}
	w.Flush()

	log.Printf("wrote cumulative committers analysis to %s", f.Name())
	return nil
}

func RunIssues(c *fetch.Context, issues []*fetch.IssueComment) error {
	log.Printf("running all issues analysis")

	// Create a map to track how many unique users have commented on issues in
	// any given month.
	commentersByMonth := map[string](*map[string]bool){}

	for _, issueComment := range issues {
		monthKey := fmt.Sprintf("%d-%d", issueComment.Date.Year(), issueComment.Date.Month())

		monthMap, ok := commentersByMonth[monthKey]
		if !ok {
			monthMap = &map[string]bool{}
			commentersByMonth[monthKey] = monthMap
		}

		// As with the above, we can blindly insert this into the map because we
		// just mark it as true even if one person has commented more than once.
		(*monthMap)[issueComment.Author] = true
	}

	now := time.Now()
	// Go's very nitpicky about these types, but these are just typecasts over
	// int. Unclear if there are any typesafety issues with this?
	endYear := int(now.Year())
	endMonth := int(now.Month())

	// TODO(vikram): Make this a paramter. Currently hardcoded to when Aqueduct
	// was released.
	year := 2022
	month := 5

	// Set up the CSV that we're going to write the results to.
	f, err := createFile(c, "cumulative_commenters.csv")
	if err != nil {
		return fmt.Errorf("failed to create file: %s", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write([]string{"Month", "Total", "Cumulative"}); err != nil {
		return fmt.Errorf("failed to write to CSV: %s", err)
	}

	// TODO(vikram): This logic is basically the same as the logic above for
	// committers. If we just distill this down to pulling out the user data from
	// the structs, this can probably go into a helper function and be deduped.
	allCommenters := map[string]bool{}
	for year <= endYear && month <= endMonth {
		// TODO(vikram): Is it better to keep this as a string or to store it as a
		// datetime? The latter I think will make it hard for grouping, which is
		// why its currently a string.
		key := fmt.Sprintf("%d-%d", year, month)
		monthCommenters, ok := commentersByMonth[key]

		// Increment the month we're on and check if we've rolled over to the next
		// year. We do this before we check if there are any commenters to avoid
		// infinite loops.
		month += 1
		if month > 12 {
			month = 1
			year += 1
		}

		if !ok { // There were no commenters this month. Unlikely but just in case?
			continue
		}

		for commenter := range *monthCommenters {
			// We can blindly add it to the map. It doesn't matter if we re-add
			// because it just sets the value in the map to `true` again.
			allCommenters[commenter] = true
		}

		row := []string{key, fmt.Sprintf("%d", len(*monthCommenters)), fmt.Sprintf("%d", len(allCommenters))}
		if err := w.Write(row); err != nil {
			return fmt.Errorf("failed to write to CSV: %s", err)
		}

	}
	w.Flush()

	log.Printf("wrote cumulative commenters analysis to %s", f.Name())
	return nil
}

// RunCumulativeStars creates a table of date and cumulative
// star count for the provided stargazers.
func RunCumulativeStars(c *fetch.Context, sg []*fetch.Stargazer) error {
	log.Printf("running cumulative stars analysis")

	// Open file and prepare.
	f, err := createFile(c, "cumulative_stars.csv")
	if err != nil {
		return fmt.Errorf("failed to create file: %s", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write([]string{"Date", "New", "Cumulative"}); err != nil {
		return fmt.Errorf("failed to write to CSV: %s", err)
	}

	// Sort the stargazers.
	slice := Stargazers(sg)
	sort.Sort(slice)

	// Now accumulate by days.
	lastDay := int64(0)
	total := 0
	count := 0
	for _, s := range slice {
		t, err := time.Parse(time.RFC3339, s.StarredAt)
		if err != nil {
			return err
		}
		day := t.Unix() / int64(60*60*24)
		if day != lastDay {
			if count > 0 {
				t := time.Unix(lastDay*60*60*24, 0)
				if err := w.Write([]string{t.Format("01/02/2006"), strconv.Itoa(count), strconv.Itoa(total)}); err != nil {
					return fmt.Errorf("failed to write to CSV: %s", err)
				}
			}
			lastDay = day
			count = 1
		} else {
			count++
		}
		total++
	}
	if count > 0 {
		t := time.Unix(lastDay*60*60*24, 0)
		if err := w.Write([]string{t.Format("01/02/2006"), strconv.Itoa(count), strconv.Itoa(total)}); err != nil {
			return fmt.Errorf("failed to write to CSV: %s", err)
		}
	}
	w.Flush()
	log.Printf("wrote cumulative stars analysis to %s", f.Name())

	return nil
}

// RunCorrelatedRepos creates a map from repo name to count of
// repos for repo lists of each stargazer.
func RunCorrelatedRepos(c *fetch.Context, listType string, sg []*fetch.Stargazer, rs map[string]*fetch.Repo) error {
	log.Printf("running correlated starred repos analysis")

	// Open file and prepare.
	f, err := createFile(c, fmt.Sprintf("correlated_%s_repos.csv", listType))
	if err != nil {
		return fmt.Errorf("failed to create file: %s", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write([]string{"Repository", "URL", "Count", "Committers", "Commits", "Additions", "Deletions"}); err != nil {
		return fmt.Errorf("failed to write to CSV: %s", err)
	}
	// Compute counts.
	counts := map[string]int{}
	for _, s := range sg {
		repos := s.Starred
		if listType == "subscribed" {
			repos = s.Subscribed
		}
		for _, rName := range repos {
			counts[rName]++
		}
	}
	// Sort repos by count.
	repos := RepoCounts{}
	for rName, count := range counts {
		repos = append(repos, &RepoCount{name: rName, count: count})
	}
	sort.Sort(repos)
	// Output repos by count (respecting minimum threshold).
	for i, r := range repos {
		if i > nMostCorrelated {
			break
		}
		c, a, d := rs[r.name].TotalCommits()
		url := fmt.Sprintf("https://github.com/%s", rs[r.name].FullName)
		if err := w.Write([]string{r.name, url, strconv.Itoa(r.count), strconv.Itoa(len(rs[r.name].Statistics)),
			strconv.Itoa(c), strconv.Itoa(a), strconv.Itoa(d)}); err != nil {
			return fmt.Errorf("failed to write to CSV: %s", err)
		}
	}
	w.Flush()
	log.Printf("wrote correlated %s repos analysis to %s", listType, f.Name())

	// Open histogram file.
	fHist, err := createFile(c, fmt.Sprintf("correlated_%s_repos_hist.csv", listType))
	if err != nil {
		return fmt.Errorf("failed to create file: %s", err)
	}
	defer fHist.Close()
	wHist := csv.NewWriter(fHist)
	if err := wHist.Write([]string{"Correlation", "Count"}); err != nil {
		return fmt.Errorf("failed to write to CSV: %s", err)
	}
	lastCorrelation := 0
	count := 0
	for _, r := range repos {
		if lastCorrelation != r.count {
			if count > 0 {
				if err := wHist.Write([]string{strconv.Itoa(lastCorrelation), strconv.Itoa(count)}); err != nil {
					return fmt.Errorf("failed to write to CSV: %s", err)
				}
			}
			lastCorrelation = r.count
			count = 1
		} else {
			count++
		}
	}
	if count > 0 {
		if err := wHist.Write([]string{strconv.Itoa(lastCorrelation), strconv.Itoa(count)}); err != nil {
			return fmt.Errorf("failed to write to CSV: %s", err)
		}
	}
	wHist.Flush()
	log.Printf("wrote correlated %s repos histogram to %s", listType, fHist.Name())

	return nil
}

// RunFollowers computes the size of follower networks, as well as
// the count of shared followers.
func RunFollowers(c *fetch.Context, sg []*fetch.Stargazer) error {
	log.Printf("running followers analysis")

	// Open file and prepare.
	f, err := createFile(c, "followers.csv")
	if err != nil {
		return fmt.Errorf("failed to create file: %s", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write([]string{"Name", "Login", "URL", "Avatar URL", "Company", "Location", "Followers", "Shared Followers"}); err != nil {
		return fmt.Errorf("failed to write to CSV: %s", err)
	}

	shared := map[string]int{}
	for _, s := range sg {
		for _, f := range s.Followers {
			shared[f.Login]++
		}
	}

	// For each stargazer, output followers, and shared followers.
	// Now accumulate by days.
	for _, s := range sg {
		sharedCount := 0
		for _, f := range s.Followers {
			if c := shared[f.Login]; c > 1 {
				sharedCount++
			}
		}
		url := fmt.Sprintf("https://github.com/%s", s.Login)
		if err := w.Write([]string{s.Name, s.Login, url, s.AvatarURL, s.Company, s.Location, strconv.Itoa(s.User.Followers), strconv.Itoa(sharedCount)}); err != nil {
			return fmt.Errorf("failed to write to CSV: %s", err)
		}
	}
	w.Flush()
	log.Printf("wrote followers analysis to %s", f.Name())

	return nil
}

// RunCommitters lists stargazers by commits to subscribed repos, from
// most prolific committer to least.
func RunCommitters(c *fetch.Context, sg []*fetch.Stargazer, rs map[string]*fetch.Repo) error {
	log.Printf("running committers analysis")

	// Open file and prepare.
	f, err := createFile(c, "committers.csv")
	if err != nil {
		return fmt.Errorf("failed to create file: %s", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write([]string{"Login", "Email", "Commits", "Additions", "Deletions"}); err != nil {
		return fmt.Errorf("failed to write to CSV: %s", err)
	}

	// Sort the stargazers.
	slice := Contributors(sg)
	sort.Sort(slice)

	// Now accumulate by days.
	for _, s := range slice {
		c, a, d := s.TotalCommits()
		if c == 0 {
			break
		}
		if err := w.Write([]string{s.Login, s.Email, strconv.Itoa(c), strconv.Itoa(a), strconv.Itoa(d)}); err != nil {
			return fmt.Errorf("failed to write to CSV: %s", err)
		}
	}
	w.Flush()
	log.Printf("wrote committers analysis to %s", f.Name())

	return nil
}

// RunCumulativeStars creates a table of date and cumulative
// star count for the provided stargazers.
func RunAttributesByTime(c *fetch.Context, sg []*fetch.Stargazer, rs map[string]*fetch.Repo) error {
	log.Printf("running stargazer attributes by time analysis")

	// Open file and prepare.
	f, err := createFile(c, "attributes_by_time.csv")
	if err != nil {
		return fmt.Errorf("failed to create file: %s", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write([]string{"Date", "New Stars", "Avg Age", "Avg Followers", "Avg Commits"}); err != nil {
		return fmt.Errorf("failed to write to CSV: %s", err)
	}

	output := func(day int64, count, age, followers, commits int) error {
		t := time.Unix(day*60*60*24, 0)
		avgAge := fmt.Sprintf("%.2f", float64(age)/float64(count))
		avgFollowers := fmt.Sprintf("%.2f", float64(followers)/float64(count))
		avgCommits := fmt.Sprintf("%.2f", float64(commits)/float64(count))
		if err := w.Write([]string{t.Format("01/02/2006"), strconv.Itoa(count), avgAge, avgFollowers, avgCommits}); err != nil {
			return fmt.Errorf("failed to write to CSV: %s", err)
		}
		return nil
	}

	const daySeconds = 60 * 60 * 24

	// Sort the stargazers.
	slice := Stargazers(sg)
	sort.Sort(slice)

	// Accumulation factor means the count of days over which to average each sample.
	factor := int64(7) // weekly

	// Now accumulate by days.
	firstDay := int64(0)
	lastDay := int64(0)
	count, age, followers, commits := 0, 0, 0, 0
	for _, s := range slice {
		t, err := time.Parse(time.RFC3339, s.StarredAt)
		if err != nil {
			return err
		}
		day := t.Unix() / daySeconds
		if firstDay == 0 {
			firstDay = day
		}
		if day != lastDay && (day-firstDay)%factor == 0 {
			if count > 0 {
				if err := output(lastDay, count, age, followers, commits); err != nil {
					return err
				}
			}
			lastDay = day
			count = 1
			age = int(s.Age() / daySeconds)
			followers = len(s.Followers)
			commits, _, _ = s.TotalCommits()
		} else {
			count++
			age += int(s.Age() / daySeconds)
			followers += len(s.Followers)
			c, _, _ := s.TotalCommits()
			commits += c
		}
	}
	if count > 0 {
		if err := output(lastDay, count, age, followers, commits); err != nil {
			return err
		}
	}
	w.Flush()
	log.Printf("wrote stargazer attributes by time analysis to %s", f.Name())

	return nil
}

func createFile(c *fetch.Context, baseName string) (*os.File, error) {
	filename := filepath.Join(c.CacheDir, c.Repo, baseName)
	f, err := os.Create(filename)
	if err != nil {
		return nil, err
	}
	return f, nil
}
