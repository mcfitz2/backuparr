package integration_tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"backuparr/internal/prowlarr"
	"backuparr/internal/radarr"
	"backuparr/internal/sonarr"
)

// TestRestoreValidationSonarr tests that restoring a backup properly restores the original state
// by adding a series, backing up, adding more series, then restoring and verifying
func TestRestoreValidationSonarr(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	// Find a Sonarr instance to test with (prefer SQLite for speed)
	var inst testInstance
	for _, i := range testInstances {
		if i.appType == "sonarr" && !i.isPostgres {
			inst = i
			break
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	apiKey, err := getAPIKeyFromContainer(inst.containerName)
	if err != nil {
		t.Fatalf("failed to get API key: %v", err)
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}

	// Helper to get all series
	getSeries := func() ([]sonarr.SeriesResource, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", inst.url+"/api/v3/series", nil)
		req.Header.Set("X-Api-Key", apiKey)
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var series []sonarr.SeriesResource
		if err := json.NewDecoder(resp.Body).Decode(&series); err != nil {
			return nil, err
		}
		return series, nil
	}

	// Helper to add a series by TVDB ID
	addSeries := func(tvdbID int32) (*sonarr.SeriesResource, error) {
		// First lookup the series
		req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/v3/series/lookup?term=tvdb:%d", inst.url, tvdbID), nil)
		req.Header.Set("X-Api-Key", apiKey)
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("lookup failed: %w", err)
		}
		defer resp.Body.Close()

		var lookupResults []sonarr.SeriesResource
		if err := json.NewDecoder(resp.Body).Decode(&lookupResults); err != nil {
			return nil, fmt.Errorf("lookup decode failed: %w", err)
		}

		if len(lookupResults) == 0 {
			return nil, fmt.Errorf("no series found for tvdb:%d", tvdbID)
		}

		series := lookupResults[0]
		monitored := true
		series.Monitored = &monitored
		qualityProfileID := int32(1)
		series.QualityProfileId = &qualityProfileID
		rootFolder := "/tv"
		series.RootFolderPath = &rootFolder
		seasonFolder := true
		series.SeasonFolder = &seasonFolder
		searchMissing := false
		series.AddOptions = &sonarr.AddSeriesOptions{
			SearchForMissingEpisodes: &searchMissing,
		}

		body, _ := json.Marshal(series)
		req, _ = http.NewRequestWithContext(ctx, "POST", inst.url+"/api/v3/series", bytes.NewReader(body))
		req.Header.Set("X-Api-Key", apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err = httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("add failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			respBody, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("add failed with status %d: %s", resp.StatusCode, string(respBody))
		}

		var added sonarr.SeriesResource
		if err := json.NewDecoder(resp.Body).Decode(&added); err != nil {
			return nil, fmt.Errorf("add decode failed: %w", err)
		}

		return &added, nil
	}

	// Helper to delete a series
	deleteSeries := func(id int32) error {
		req, _ := http.NewRequestWithContext(ctx, "DELETE", fmt.Sprintf("%s/api/v3/series/%d?deleteFiles=true", inst.url, id), nil)
		req.Header.Set("X-Api-Key", apiKey)
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}

	// Ensure root folder exists
	ensureRootFolder := func(path string) {
		body := fmt.Sprintf(`{"path": %q}`, path)
		req, _ := http.NewRequestWithContext(ctx, "POST", inst.url+"/api/v3/rootfolder", bytes.NewReader([]byte(body)))
		req.Header.Set("X-Api-Key", apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, _ := httpClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}

	// Step 0: Ensure root folder exists
	ensureRootFolder("/tv")

	// Step 1: Clean up any existing series
	t.Log("Step 1: Cleaning up existing series...")
	existingSeries, err := getSeries()
	if err != nil {
		t.Fatalf("failed to get existing series: %v", err)
	}
	for _, s := range existingSeries {
		if s.Id != nil {
			_ = deleteSeries(*s.Id)
		}
	}

	// Step 2: Add first series (Breaking Bad - TVDB 81189)
	t.Log("Step 2: Adding first series (Breaking Bad)...")
	series1, err := addSeries(81189)
	if err != nil {
		t.Fatalf("failed to add first series: %v", err)
	}
	t.Logf("Added series: %s (ID: %d)", *series1.Title, *series1.Id)

	// Verify we have 1 series
	seriesAfterAdd1, err := getSeries()
	if err != nil {
		t.Fatalf("failed to get series: %v", err)
	}
	if len(seriesAfterAdd1) != 1 {
		t.Fatalf("expected 1 series, got %d", len(seriesAfterAdd1))
	}

	// Step 3: Create a backup
	t.Log("Step 3: Creating backup...")
	client := createClient(t, inst)
	_, reader, err := client.Backup(ctx)
	if err != nil {
		t.Fatalf("failed to create backup: %v", err)
	}
	backupData, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatalf("failed to read backup: %v", err)
	}
	t.Logf("Backup created: %d bytes", len(backupData))

	// Step 4: Add second series (Game of Thrones - TVDB 121361)
	t.Log("Step 4: Adding second series (Game of Thrones)...")
	series2, err := addSeries(121361)
	if err != nil {
		t.Fatalf("failed to add second series: %v", err)
	}
	t.Logf("Added series: %s (ID: %d)", *series2.Title, *series2.Id)

	// Step 5: Add third series (The Office - TVDB 73244)
	t.Log("Step 5: Adding third series (The Office)...")
	series3, err := addSeries(73244)
	if err != nil {
		t.Fatalf("failed to add third series: %v", err)
	}
	t.Logf("Added series: %s (ID: %d)", *series3.Title, *series3.Id)

	// Verify we now have 3 series
	seriesAfterAdd3, err := getSeries()
	if err != nil {
		t.Fatalf("failed to get series: %v", err)
	}
	if len(seriesAfterAdd3) != 3 {
		t.Fatalf("expected 3 series, got %d", len(seriesAfterAdd3))
	}
	t.Logf("Current state: %d series before restore", len(seriesAfterAdd3))

	// Step 6: Restore from backup
	t.Log("Step 6: Restoring from backup...")
	err = client.Restore(ctx, bytes.NewReader(backupData))
	if err != nil {
		t.Fatalf("failed to restore: %v", err)
	}

	// Step 7: Wait for restart
	t.Log("Step 7: Waiting for app to restart...")
	time.Sleep(15 * time.Second)

	// Wait for app to come back online
	for i := 0; i < 20; i++ {
		_, err := getSeries()
		if err == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}

	// Step 8: Verify the original state is restored (only 1 series)
	t.Log("Step 8: Verifying restored state...")
	seriesAfterRestore, err := getSeries()
	if err != nil {
		t.Fatalf("failed to get series after restore: %v", err)
	}

	if len(seriesAfterRestore) != 1 {
		t.Errorf("VALIDATION FAILED: expected 1 series after restore, got %d", len(seriesAfterRestore))
		for _, s := range seriesAfterRestore {
			t.Logf("  - %s (ID: %d)", *s.Title, *s.Id)
		}
	} else {
		t.Logf("VALIDATION PASSED: Restored to original state with 1 series")
		if seriesAfterRestore[0].TvdbId != nil && *seriesAfterRestore[0].TvdbId == 81189 {
			t.Logf("  - Verified: %s (TVDB: %d)", *seriesAfterRestore[0].Title, *seriesAfterRestore[0].TvdbId)
		} else {
			t.Errorf("VALIDATION FAILED: Expected Breaking Bad (TVDB 81189), got %s", *seriesAfterRestore[0].Title)
		}
	}
}

// TestRestoreValidationRadarr tests that restoring a backup properly restores the original state
// by adding a movie, backing up, adding more movies, then restoring and verifying
func TestRestoreValidationRadarr(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	// Find a Radarr instance to test with (prefer SQLite for speed)
	var inst testInstance
	for _, i := range testInstances {
		if i.appType == "radarr" && !i.isPostgres {
			inst = i
			break
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	apiKey, err := getAPIKeyFromContainer(inst.containerName)
	if err != nil {
		t.Fatalf("failed to get API key: %v", err)
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}

	// Helper to get all movies
	getMovies := func() ([]radarr.MovieResource, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", inst.url+"/api/v3/movie", nil)
		req.Header.Set("X-Api-Key", apiKey)
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var movies []radarr.MovieResource
		if err := json.NewDecoder(resp.Body).Decode(&movies); err != nil {
			return nil, err
		}
		return movies, nil
	}

	// Helper to add a movie by TMDB ID
	addMovie := func(tmdbID int32) (*radarr.MovieResource, error) {
		// First lookup the movie
		req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/v3/movie/lookup?term=tmdb:%d", inst.url, tmdbID), nil)
		req.Header.Set("X-Api-Key", apiKey)
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("lookup failed: %w", err)
		}
		defer resp.Body.Close()

		var lookupResults []radarr.MovieResource
		if err := json.NewDecoder(resp.Body).Decode(&lookupResults); err != nil {
			return nil, fmt.Errorf("lookup decode failed: %w", err)
		}

		if len(lookupResults) == 0 {
			return nil, fmt.Errorf("no movie found for tmdb:%d", tmdbID)
		}

		movie := lookupResults[0]
		monitored := true
		movie.Monitored = &monitored
		qualityProfileID := int32(1)
		movie.QualityProfileId = &qualityProfileID
		rootFolder := "/movies"
		movie.RootFolderPath = &rootFolder
		minAvail := radarr.Announced
		movie.MinimumAvailability = &minAvail
		searchForMovie := false
		movie.AddOptions = &radarr.AddMovieOptions{
			SearchForMovie: &searchForMovie,
		}

		body, _ := json.Marshal(movie)
		req, _ = http.NewRequestWithContext(ctx, "POST", inst.url+"/api/v3/movie", bytes.NewReader(body))
		req.Header.Set("X-Api-Key", apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err = httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("add failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			respBody, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("add failed with status %d: %s", resp.StatusCode, string(respBody))
		}

		var added radarr.MovieResource
		if err := json.NewDecoder(resp.Body).Decode(&added); err != nil {
			return nil, fmt.Errorf("add decode failed: %w", err)
		}

		return &added, nil
	}

	// Helper to delete a movie
	deleteMovie := func(id int32) error {
		req, _ := http.NewRequestWithContext(ctx, "DELETE", fmt.Sprintf("%s/api/v3/movie/%d?deleteFiles=true", inst.url, id), nil)
		req.Header.Set("X-Api-Key", apiKey)
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}

	// Ensure root folder exists
	ensureRootFolder := func(path string) error {
		body := fmt.Sprintf(`{"path": %q}`, path)
		req, _ := http.NewRequestWithContext(ctx, "POST", inst.url+"/api/v3/rootfolder", bytes.NewReader([]byte(body)))
		req.Header.Set("X-Api-Key", apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil // Ignore errors - folder may already exist
	}

	// Step 0: Ensure root folder exists
	_ = ensureRootFolder("/movies")

	// Step 1: Clean up any existing movies
	t.Log("Step 1: Cleaning up existing movies...")
	existingMovies, err := getMovies()
	if err != nil {
		t.Fatalf("failed to get existing movies: %v", err)
	}
	for _, m := range existingMovies {
		if m.Id != nil {
			_ = deleteMovie(*m.Id)
		}
	}

	// Step 2: Add first movie (The Matrix - TMDB 603)
	t.Log("Step 2: Adding first movie (The Matrix)...")
	movie1, err := addMovie(603)
	if err != nil {
		t.Fatalf("failed to add first movie: %v", err)
	}
	t.Logf("Added movie: %s (ID: %d)", *movie1.Title, *movie1.Id)

	// Verify we have 1 movie
	moviesAfterAdd1, err := getMovies()
	if err != nil {
		t.Fatalf("failed to get movies: %v", err)
	}
	if len(moviesAfterAdd1) != 1 {
		t.Fatalf("expected 1 movie, got %d", len(moviesAfterAdd1))
	}

	// Step 3: Create a backup
	t.Log("Step 3: Creating backup...")
	client := createClient(t, inst)
	_, reader, err := client.Backup(ctx)
	if err != nil {
		t.Fatalf("failed to create backup: %v", err)
	}
	backupData, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatalf("failed to read backup: %v", err)
	}
	t.Logf("Backup created: %d bytes", len(backupData))

	// Step 4: Add second movie (Inception - TMDB 27205)
	t.Log("Step 4: Adding second movie (Inception)...")
	movie2, err := addMovie(27205)
	if err != nil {
		t.Fatalf("failed to add second movie: %v", err)
	}
	t.Logf("Added movie: %s (ID: %d)", *movie2.Title, *movie2.Id)

	// Step 5: Add third movie (The Dark Knight - TMDB 155)
	t.Log("Step 5: Adding third movie (The Dark Knight)...")
	movie3, err := addMovie(155)
	if err != nil {
		t.Fatalf("failed to add third movie: %v", err)
	}
	t.Logf("Added movie: %s (ID: %d)", *movie3.Title, *movie3.Id)

	// Verify we now have 3 movies
	moviesAfterAdd3, err := getMovies()
	if err != nil {
		t.Fatalf("failed to get movies: %v", err)
	}
	if len(moviesAfterAdd3) != 3 {
		t.Fatalf("expected 3 movies, got %d", len(moviesAfterAdd3))
	}
	t.Logf("Current state: %d movies before restore", len(moviesAfterAdd3))

	// Step 6: Restore from backup
	t.Log("Step 6: Restoring from backup...")
	err = client.Restore(ctx, bytes.NewReader(backupData))
	if err != nil {
		t.Fatalf("failed to restore: %v", err)
	}

	// Step 7: Wait for restart
	t.Log("Step 7: Waiting for app to restart...")
	time.Sleep(15 * time.Second)

	// Wait for app to come back online
	for i := 0; i < 20; i++ {
		_, err := getMovies()
		if err == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}

	// Step 8: Verify the original state is restored (only 1 movie)
	t.Log("Step 8: Verifying restored state...")
	moviesAfterRestore, err := getMovies()
	if err != nil {
		t.Fatalf("failed to get movies after restore: %v", err)
	}

	if len(moviesAfterRestore) != 1 {
		t.Errorf("VALIDATION FAILED: expected 1 movie after restore, got %d", len(moviesAfterRestore))
		for _, m := range moviesAfterRestore {
			t.Logf("  - %s (ID: %d)", *m.Title, *m.Id)
		}
	} else {
		t.Logf("VALIDATION PASSED: Restored to original state with 1 movie")
		if moviesAfterRestore[0].TmdbId != nil && *moviesAfterRestore[0].TmdbId == 603 {
			t.Logf("  - Verified: %s (TMDB: %d)", *moviesAfterRestore[0].Title, *moviesAfterRestore[0].TmdbId)
		} else {
			t.Errorf("VALIDATION FAILED: Expected The Matrix (TMDB 603), got %s", *moviesAfterRestore[0].Title)
		}
	}
}

// TestRestoreValidationPostgres tests restore validation with PostgreSQL instances specifically
func TestRestoreValidationPostgres(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	// Find PostgreSQL instances
	var sonarrPg, radarrPg *testInstance
	for i := range testInstances {
		if testInstances[i].isPostgres {
			if testInstances[i].appType == "sonarr" {
				sonarrPg = &testInstances[i]
			} else if testInstances[i].appType == "radarr" {
				radarrPg = &testInstances[i]
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Test Radarr PostgreSQL (simpler - just uses movies)
	if radarrPg != nil {
		t.Run("radarr-postgres", func(t *testing.T) {
			runRadarrValidation(t, ctx, *radarrPg)
		})
	}

	// Test Sonarr PostgreSQL
	if sonarrPg != nil {
		t.Run("sonarr-postgres", func(t *testing.T) {
			runSonarrValidation(t, ctx, *sonarrPg)
		})
	}
}

func runSonarrValidation(t *testing.T, ctx context.Context, inst testInstance) {
	apiKey, err := getAPIKeyFromContainer(inst.containerName)
	if err != nil {
		t.Fatalf("failed to get API key: %v", err)
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}

	getSeries := func() ([]sonarr.SeriesResource, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", inst.url+"/api/v3/series", nil)
		req.Header.Set("X-Api-Key", apiKey)
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var series []sonarr.SeriesResource
		json.NewDecoder(resp.Body).Decode(&series)
		return series, nil
	}

	addSeries := func(tvdbID int32) (*sonarr.SeriesResource, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/v3/series/lookup?term=tvdb:%d", inst.url, tvdbID), nil)
		req.Header.Set("X-Api-Key", apiKey)
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		var results []sonarr.SeriesResource
		json.NewDecoder(resp.Body).Decode(&results)
		if len(results) == 0 {
			return nil, fmt.Errorf("no results")
		}

		series := results[0]
		monitored := true
		series.Monitored = &monitored
		qp := int32(1)
		series.QualityProfileId = &qp
		rf := "/tv"
		series.RootFolderPath = &rf
		sf := true
		series.SeasonFolder = &sf
		search := false
		series.AddOptions = &sonarr.AddSeriesOptions{SearchForMissingEpisodes: &search}

		body, _ := json.Marshal(series)
		req, _ = http.NewRequestWithContext(ctx, "POST", inst.url+"/api/v3/series", bytes.NewReader(body))
		req.Header.Set("X-Api-Key", apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err = httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, b)
		}
		var added sonarr.SeriesResource
		json.NewDecoder(resp.Body).Decode(&added)
		return &added, nil
	}

	deleteSeries := func(id int32) {
		req, _ := http.NewRequestWithContext(ctx, "DELETE", fmt.Sprintf("%s/api/v3/series/%d?deleteFiles=true", inst.url, id), nil)
		req.Header.Set("X-Api-Key", apiKey)
		resp, _ := httpClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}

	ensureRootFolder := func(path string) {
		body := fmt.Sprintf(`{"path": %q}`, path)
		req, _ := http.NewRequestWithContext(ctx, "POST", inst.url+"/api/v3/rootfolder", bytes.NewReader([]byte(body)))
		req.Header.Set("X-Api-Key", apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, _ := httpClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}

	// Ensure root folder exists
	ensureRootFolder("/tv")

	// Cleanup
	existing, _ := getSeries()
	for _, s := range existing {
		if s.Id != nil {
			deleteSeries(*s.Id)
		}
	}

	// Add initial series
	t.Log("Adding initial series...")
	s1, err := addSeries(81189) // Breaking Bad
	if err != nil {
		t.Fatalf("failed to add series: %v", err)
	}
	t.Logf("Added: %s", *s1.Title)

	// Create backup
	t.Log("Creating backup...")
	client := createClient(t, inst)
	_, reader, err := client.Backup(ctx)
	if err != nil {
		t.Fatalf("backup failed: %v", err)
	}
	backupData, _ := io.ReadAll(reader)
	reader.Close()

	// Add more series
	t.Log("Adding additional series...")
	s2, _ := addSeries(121361) // GoT
	if s2 != nil {
		t.Logf("Added: %s", *s2.Title)
	}

	before, _ := getSeries()
	t.Logf("Before restore: %d series", len(before))

	// Restore
	t.Log("Restoring...")
	if err := client.Restore(ctx, bytes.NewReader(backupData)); err != nil {
		t.Fatalf("restore failed: %v", err)
	}

	time.Sleep(15 * time.Second)
	for i := 0; i < 20; i++ {
		if _, err := getSeries(); err == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}

	after, _ := getSeries()
	t.Logf("After restore: %d series", len(after))

	if len(after) != 1 {
		t.Errorf("expected 1 series, got %d", len(after))
	} else {
		t.Log("VALIDATION PASSED: PostgreSQL restore successful")
	}
}

func runRadarrValidation(t *testing.T, ctx context.Context, inst testInstance) {
	apiKey, err := getAPIKeyFromContainer(inst.containerName)
	if err != nil {
		t.Fatalf("failed to get API key: %v", err)
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}

	getMovies := func() ([]radarr.MovieResource, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", inst.url+"/api/v3/movie", nil)
		req.Header.Set("X-Api-Key", apiKey)
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var movies []radarr.MovieResource
		json.NewDecoder(resp.Body).Decode(&movies)
		return movies, nil
	}

	addMovie := func(tmdbID int32) (*radarr.MovieResource, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/v3/movie/lookup?term=tmdb:%d", inst.url, tmdbID), nil)
		req.Header.Set("X-Api-Key", apiKey)
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		var results []radarr.MovieResource
		json.NewDecoder(resp.Body).Decode(&results)
		if len(results) == 0 {
			return nil, fmt.Errorf("no results")
		}

		movie := results[0]
		monitored := true
		movie.Monitored = &monitored
		qp := int32(1)
		movie.QualityProfileId = &qp
		rf := "/movies"
		movie.RootFolderPath = &rf
		ma := radarr.Announced
		movie.MinimumAvailability = &ma
		search := false
		movie.AddOptions = &radarr.AddMovieOptions{SearchForMovie: &search}

		body, _ := json.Marshal(movie)
		req, _ = http.NewRequestWithContext(ctx, "POST", inst.url+"/api/v3/movie", bytes.NewReader(body))
		req.Header.Set("X-Api-Key", apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err = httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, b)
		}
		var added radarr.MovieResource
		json.NewDecoder(resp.Body).Decode(&added)
		return &added, nil
	}

	deleteMovie := func(id int32) {
		req, _ := http.NewRequestWithContext(ctx, "DELETE", fmt.Sprintf("%s/api/v3/movie/%d?deleteFiles=true", inst.url, id), nil)
		req.Header.Set("X-Api-Key", apiKey)
		resp, _ := httpClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}

	ensureRootFolder := func(path string) {
		body := fmt.Sprintf(`{"path": %q}`, path)
		req, _ := http.NewRequestWithContext(ctx, "POST", inst.url+"/api/v3/rootfolder", bytes.NewReader([]byte(body)))
		req.Header.Set("X-Api-Key", apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, _ := httpClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}

	// Ensure root folder exists
	ensureRootFolder("/movies")

	// Cleanup
	existing, _ := getMovies()
	for _, m := range existing {
		if m.Id != nil {
			deleteMovie(*m.Id)
		}
	}

	// Add initial movie
	t.Log("Adding initial movie...")
	m1, err := addMovie(603) // The Matrix
	if err != nil {
		t.Fatalf("failed to add movie: %v", err)
	}
	t.Logf("Added: %s", *m1.Title)

	// Create backup
	t.Log("Creating backup...")
	client := createClient(t, inst)
	_, reader, err := client.Backup(ctx)
	if err != nil {
		t.Fatalf("backup failed: %v", err)
	}
	backupData, _ := io.ReadAll(reader)
	reader.Close()

	// Add more movies
	t.Log("Adding additional movie...")
	m2, _ := addMovie(27205) // Inception
	if m2 != nil {
		t.Logf("Added: %s", *m2.Title)
	}

	before, _ := getMovies()
	t.Logf("Before restore: %d movies", len(before))

	// Restore
	t.Log("Restoring...")
	if err := client.Restore(ctx, bytes.NewReader(backupData)); err != nil {
		t.Fatalf("restore failed: %v", err)
	}

	time.Sleep(15 * time.Second)
	for i := 0; i < 20; i++ {
		if _, err := getMovies(); err == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}

	after, _ := getMovies()
	t.Logf("After restore: %d movies", len(after))

	if len(after) != 1 {
		t.Errorf("expected 1 movie, got %d", len(after))
	} else {
		t.Log("VALIDATION PASSED: PostgreSQL restore successful")
	}
}

// TestRestoreValidationProwlarr tests that restoring a backup properly restores the original state
// by adding a tag, backing up, adding more tags, then restoring and verifying.
// Prowlarr uses tags as a simple data entity for validation (no external lookups needed).
func TestRestoreValidationProwlarr(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test. Set INTEGRATION_TEST=1 to run.")
	}

	// Find a Prowlarr instance to test with
	var inst testInstance
	found := false
	for _, i := range testInstances {
		if i.appType == "prowlarr" {
			inst = i
			found = true
			break
		}
	}
	if !found {
		t.Skip("No Prowlarr instance configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	apiKey, err := getAPIKeyFromContainer(inst.containerName)
	if err != nil {
		t.Fatalf("failed to get API key: %v", err)
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}

	// Helper to get all tags
	getTags := func() ([]prowlarr.TagResource, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", inst.url+"/api/v1/tag", nil)
		req.Header.Set("X-Api-Key", apiKey)
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var tags []prowlarr.TagResource
		if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
			return nil, err
		}
		return tags, nil
	}

	// Helper to add a tag
	addTag := func(label string) (*prowlarr.TagResource, error) {
		tag := prowlarr.TagResource{Label: &label}
		body, _ := json.Marshal(tag)
		req, _ := http.NewRequestWithContext(ctx, "POST", inst.url+"/api/v1/tag", bytes.NewReader(body))
		req.Header.Set("X-Api-Key", apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("add tag failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			respBody, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("add tag failed with status %d: %s", resp.StatusCode, string(respBody))
		}
		var added prowlarr.TagResource
		if err := json.NewDecoder(resp.Body).Decode(&added); err != nil {
			return nil, fmt.Errorf("decode failed: %w", err)
		}
		return &added, nil
	}

	// Helper to delete a tag
	deleteTag := func(id int32) error {
		req, _ := http.NewRequestWithContext(ctx, "DELETE", fmt.Sprintf("%s/api/v1/tag/%d", inst.url, id), nil)
		req.Header.Set("X-Api-Key", apiKey)
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}

	// Step 1: Clean up any existing tags
	t.Log("Step 1: Cleaning up existing tags...")
	existingTags, err := getTags()
	if err != nil {
		t.Fatalf("failed to get existing tags: %v", err)
	}
	for _, tag := range existingTags {
		if tag.Id != nil {
			_ = deleteTag(*tag.Id)
		}
	}

	// Step 2: Add first tag
	t.Log("Step 2: Adding first tag (test-backup-tag)...")
	tag1, err := addTag("test-backup-tag")
	if err != nil {
		t.Fatalf("failed to add first tag: %v", err)
	}
	t.Logf("Added tag: %s (ID: %d)", *tag1.Label, *tag1.Id)

	// Verify we have 1 tag
	tagsAfterAdd1, err := getTags()
	if err != nil {
		t.Fatalf("failed to get tags: %v", err)
	}
	if len(tagsAfterAdd1) != 1 {
		t.Fatalf("expected 1 tag, got %d", len(tagsAfterAdd1))
	}

	// Step 3: Create a backup
	t.Log("Step 3: Creating backup...")
	client := createClient(t, inst)
	_, reader, err := client.Backup(ctx)
	if err != nil {
		t.Fatalf("failed to create backup: %v", err)
	}
	backupData, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatalf("failed to read backup: %v", err)
	}
	t.Logf("Backup created: %d bytes", len(backupData))

	// Step 4: Add second tag
	t.Log("Step 4: Adding second tag (extra-tag-1)...")
	tag2, err := addTag("extra-tag-1")
	if err != nil {
		t.Fatalf("failed to add second tag: %v", err)
	}
	t.Logf("Added tag: %s (ID: %d)", *tag2.Label, *tag2.Id)

	// Step 5: Add third tag
	t.Log("Step 5: Adding third tag (extra-tag-2)...")
	tag3, err := addTag("extra-tag-2")
	if err != nil {
		t.Fatalf("failed to add third tag: %v", err)
	}
	t.Logf("Added tag: %s (ID: %d)", *tag3.Label, *tag3.Id)

	// Verify we now have 3 tags
	tagsAfterAdd3, err := getTags()
	if err != nil {
		t.Fatalf("failed to get tags: %v", err)
	}
	if len(tagsAfterAdd3) != 3 {
		t.Fatalf("expected 3 tags, got %d", len(tagsAfterAdd3))
	}
	t.Logf("Current state: %d tags before restore", len(tagsAfterAdd3))

	// Step 6: Restore from backup
	t.Log("Step 6: Restoring from backup...")
	err = client.Restore(ctx, bytes.NewReader(backupData))
	if err != nil {
		t.Fatalf("failed to restore: %v", err)
	}

	// Step 7: Wait for restart
	t.Log("Step 7: Waiting for app to restart...")
	time.Sleep(15 * time.Second)

	// Wait for app to come back online
	for i := 0; i < 20; i++ {
		_, err := getTags()
		if err == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}

	// Step 8: Verify the original state is restored (only 1 tag)
	t.Log("Step 8: Verifying restored state...")
	tagsAfterRestore, err := getTags()
	if err != nil {
		t.Fatalf("failed to get tags after restore: %v", err)
	}

	if len(tagsAfterRestore) != 1 {
		t.Errorf("VALIDATION FAILED: expected 1 tag after restore, got %d", len(tagsAfterRestore))
		for _, tag := range tagsAfterRestore {
			t.Logf("  - %s (ID: %d)", *tag.Label, *tag.Id)
		}
	} else {
		t.Logf("VALIDATION PASSED: Restored to original state with 1 tag")
		if tagsAfterRestore[0].Label != nil && *tagsAfterRestore[0].Label == "test-backup-tag" {
			t.Logf("  - Verified: %s", *tagsAfterRestore[0].Label)
		} else {
			label := ""
			if tagsAfterRestore[0].Label != nil {
				label = *tagsAfterRestore[0].Label
			}
			t.Errorf("VALIDATION FAILED: Expected tag 'test-backup-tag', got %q", label)
		}
	}
}
