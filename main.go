// Package main contains the IAASB document downloader.
package main

// Import the packages required by this program.
import (
	// Import crypto/sha256 to identify identical PDF files.
	"crypto/sha256"
	// Import encoding/hex to convert SHA-256 hashes into strings.
	"encoding/hex"
	// Import errors to check filesystem errors.
	"errors"
	// Import fmt to build URLs and formatted error messages.
	"fmt"
	// Import io to copy downloaded files.
	"io"
	// Import log/slog for structured application logging.
	"log/slog"
	// Import net/http to make HTTP requests.
	"net/http"
	// Import net/url to parse and resolve URLs.
	"net/url"
	// Import os for filesystem operations.
	"os"
	// Import path/filepath for filesystem-safe paths.
	"path/filepath"
	// Import strings for string processing.
	"strings"
	// Import sync to protect shared crawler state.
	"sync"
	// Import time to add delays between requests.
	"time"

	// Import the HTML parser used to inspect IAASB pages.
	"golang.org/x/net/html"
)

// standardsPageURLs contains all IAASB standards and support resource listing URLs.
var standardsPageURLs = []string{
	// Crawl the IAASB Support & Resources listing.
	"https://www.iaasb.org/support-resources?language=399&page=%d",
	// Crawl the IAASB Standards & Pronouncements listing.
	"https://www.iaasb.org/standards-pronouncements?language=399&page=%d",
}

// pastMeetingsPageURL contains the paginated IAASB Past Meetings listing URL.
const pastMeetingsPageURL = "https://www.iaasb.org/meetings/past-meetings?page=%d"

// pdfDirectory is the directory where PDFs will be saved.
const pdfDirectory = "PDFs"

// crawlerUserAgent identifies this program to the IAASB server.
const crawlerUserAgent = "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

// maximumWorkers controls the maximum number of workers available for future concurrent processing.
const maximumWorkers = 5

// requestDelay controls the delay between HTTP requests.
const requestDelay = 750 * time.Millisecond

// httpClient is the shared HTTP client used by the crawler.
var httpClient = &http.Client{
	// Set a reasonable timeout for every HTTP request.
	Timeout: 2 * time.Minute,
}

// invalidFilenameCharacters contains characters that are unsafe in filenames.
var invalidFilenameCharacters = strings.NewReplacer(
	// Replace the less-than character.
	"<", "_",
	// Replace the greater-than character.
	">", "_",
	// Replace the colon character.
	":", "_",
	// Replace the double-quote character.
	"\"", "_",
	// Replace the forward-slash character.
	"/", "_",
	// Replace the backslash character.
	"\\", "_",
	// Replace the pipe character.
	"|", "_",
	// Replace the question-mark character.
	"?", "_",
	// Replace the asterisk character.
	"*", "_",
)

// Publication represents one IAASB publication or meeting page.
type Publication struct {
	// URL contains the page URL.
	URL string
	// Title contains the page title.
	Title string
}

// PDFDocument represents one PDF discovered on an IAASB page.
type PDFDocument struct {
	// URL contains the PDF download URL.
	URL string
	// SourcePageURL contains the page where the PDF was found.
	SourcePageURL string
	// FileName contains the desired local filename.
	FileName string
}

// IAASBCrawler contains the crawler's in-memory state.
type IAASBCrawler struct {
	// OutputDirectory contains the directory where PDFs are stored.
	OutputDirectory string
	// VisitedPublicationPages contains pages already scraped.
	VisitedPublicationPages map[string]struct{}
	// ProcessedPDFURLs contains PDF URLs already processed.
	ProcessedPDFURLs map[string]struct{}
	// Mutex protects the crawler's shared maps.
	Mutex sync.Mutex
}

// createCrawler creates a new IAASB crawler.
func createCrawler(outputDirectory string) *IAASBCrawler {
	// Return a crawler with initialized state.
	return &IAASBCrawler{
		// Set the PDF output directory.
		OutputDirectory: outputDirectory,
		// Create the visited page set.
		VisitedPublicationPages: make(map[string]struct{}),
		// Create the processed PDF URL set.
		ProcessedPDFURLs: make(map[string]struct{}),
	}
}

// main starts the IAASB crawler.
func main() {
	// Create the PDFs directory when it does not already exist.
	if err := os.MkdirAll(pdfDirectory, 0o755); err != nil {
		// Log the directory creation error.
		slog.Error("unable to create PDF directory", "error", err)
		// Exit with a failure status.
		os.Exit(1)
	}

	// Create the IAASB crawler.
	crawler := createCrawler(pdfDirectory)

	// Crawl the Support & Resources and Standards & Pronouncements pages.
	if err := crawler.crawlStandardsPages(); err != nil {
		// Log the standards crawler error.
		slog.Error("standards crawler failed", "error", err)
		// Exit with a failure status.
		os.Exit(1)
	}

	// Crawl the IAASB Past Meetings pages.
	if err := crawler.crawlPastMeetings(); err != nil {
		// Log the meetings crawler error.
		slog.Error("past meetings crawler failed", "error", err)
		// Exit with a failure status.
		os.Exit(1)
	}

	// Report successful completion.
	slog.Info("IAASB crawler completed successfully")
}

// crawlStandardsPages visits all configured IAASB standards and support resource listings.
func (crawler *IAASBCrawler) crawlStandardsPages() error {
	// Keep one shared set so duplicate pages across both IAASB listings are not processed twice.
	knownPublicationPages := make(map[string]struct{})

	// Process every configured IAASB listing URL.
	for _, standardsPageURL := range standardsPageURLs {
		// Start crawling at page zero.
		pageNumber := 0

		// Continue until the current listing contains no new publication URLs.
		for {
			// Build the current listing URL.
			currentPageURL := fmt.Sprintf(standardsPageURL, pageNumber)

			// Log the listing page being visited.
			slog.Info("visiting IAASB listing page", "page", pageNumber, "url", currentPageURL)

			// Find all publication pages on the current listing page.
			publications, err := crawler.findPublicationsOnListingPage(currentPageURL)
			if err != nil {
				// Return the listing page error.
				return fmt.Errorf("find publications on page %d: %w", pageNumber, err)
			}

			// Count the new publication pages found on this listing page.
			newPublicationCount := 0

			// Process every publication found on the current listing page.
			for _, publication := range publications {
				// Check whether this publication was already discovered on either listing.
				if _, exists := knownPublicationPages[publication.URL]; exists {
					// Skip the duplicate publication.
					continue
				}

				// Remember this publication URL globally for this run.
				knownPublicationPages[publication.URL] = struct{}{}

				// Increment the new publication counter.
				newPublicationCount++

				// Visit the publication immediately and download its PDFs.
				if err := crawler.processPublication(publication); err != nil {
					// Log the error while continuing with other publications.
					slog.Error("failed to process publication", "title", publication.Title, "url", publication.URL, "error", err)
				}
			}

			// Log the current listing page results.
			slog.Info("IAASB listing page completed", "page", pageNumber, "url", currentPageURL, "publications_on_page", len(publications), "new_publications", newPublicationCount, "total_unique_publications", len(knownPublicationPages))

			// Stop the current listing when no new publication pages were found.
			if newPublicationCount == 0 {
				// Log that this listing has finished.
				slog.Info("no new publication URLs found; moving to next listing", "url", currentPageURL)
				// Stop crawling this listing.
				break
			}

			// Move to the next listing page.
			pageNumber++

			// Wait before making another request.
			time.Sleep(requestDelay)
		}
	}

	// Return successfully after processing both listing sources.
	return nil
}

// crawlPastMeetings visits every paginated IAASB Past Meetings listing.
func (crawler *IAASBCrawler) crawlPastMeetings() error {
	// Keep track of every unique meeting URL discovered during this run.
	knownMeetingPages := make(map[string]struct{})

	// Start at the first Past Meetings page.
	pageNumber := 0

	// Continue until a Past Meetings page contains no new meetings.
	for {
		// Build the current Past Meetings URL.
		currentPageURL := fmt.Sprintf(pastMeetingsPageURL, pageNumber)

		// Log the Past Meetings page being visited.
		slog.Info("visiting IAASB Past Meetings page", "page", pageNumber, "url", currentPageURL)

		// Find every meeting page on the current listing.
		meetings, err := crawler.findMeetingsOnListingPage(currentPageURL)
		if err != nil {
			// Return the Past Meetings listing error.
			return fmt.Errorf("find meetings on page %d: %w", pageNumber, err)
		}

		// Count new meeting pages found on this page.
		newMeetingCount := 0

		// Process every meeting discovered on the current page.
		for _, meeting := range meetings {
			// Check whether this meeting page was already discovered.
			if _, exists := knownMeetingPages[meeting.URL]; exists {
				// Skip the duplicate meeting.
				continue
			}

			// Remember this meeting page globally.
			knownMeetingPages[meeting.URL] = struct{}{}

			// Increment the new meeting counter.
			newMeetingCount++

			// Visit the meeting page and download every PDF immediately.
			if err := crawler.processPublication(meeting); err != nil {
				// Log the error while continuing with other meetings.
				slog.Error("failed to process meeting", "title", meeting.Title, "url", meeting.URL, "error", err)
			}
		}

		// Log the current Past Meetings page results.
		slog.Info("IAASB Past Meetings page completed", "page", pageNumber, "url", currentPageURL, "meetings_on_page", len(meetings), "new_meetings", newMeetingCount, "total_unique_meetings", len(knownMeetingPages))

		// Stop when the current page contains no new meetings.
		if newMeetingCount == 0 {
			// Log that Past Meetings crawling has finished.
			slog.Info("no new meeting URLs found; stopping Past Meetings crawler", "url", currentPageURL)
			// Stop crawling Past Meetings.
			break
		}

		// Move to the next Past Meetings page.
		pageNumber++

		// Wait before requesting the next page.
		time.Sleep(requestDelay)
	}

	// Return successfully.
	return nil
}

// findPublicationsOnListingPage extracts publication URLs from one standards or support listing page.
func (crawler *IAASBCrawler) findPublicationsOnListingPage(listingPageURL string) ([]Publication, error) {
	// Download the listing page.
	responseBody, err := crawler.fetchURL(listingPageURL)
	if err != nil {
		// Return the HTTP error.
		return nil, err
	}

	// Close the response body after parsing it.
	defer responseBody.Close()

	// Parse the HTML document.
	htmlDocument, err := html.Parse(responseBody)
	if err != nil {
		// Return the HTML parsing error.
		return nil, fmt.Errorf("parse HTML: %w", err)
	}

	// Store publications discovered on this page.
	var publications []Publication

	// Prevent duplicate publication URLs on this page.
	foundPublicationURLs := make(map[string]struct{})

	// Define the recursive HTML tree walker.
	var walkHTML func(*html.Node)

	// Define the HTML tree walker.
	walkHTML = func(currentNode *html.Node) {
		// Stop when the node is nil.
		if currentNode == nil {
			// Stop processing this branch.
			return
		}

		// Check whether the current node is an anchor element.
		if currentNode.Type == html.ElementNode && currentNode.Data == "a" {
			// Convert anchor attributes into a map.
			attributes := convertAttributesToMap(currentNode.Attr)

			// Read the anchor CSS class.
			anchorClass := attributes["class"]

			// Read the anchor URL.
			rawPublicationURL := strings.TrimSpace(attributes["href"])

			// Process IAASB publication card links.
			if strings.Contains(anchorClass, "card__link-wrap") && rawPublicationURL != "" {
				// Resolve the publication URL against the listing URL.
				absolutePublicationURL, resolveError := resolveURL(listingPageURL, rawPublicationURL)

				// Process only valid IAASB publication URLs.
				if resolveError == nil && isIAASBPublicationURL(absolutePublicationURL) {
					// Check whether this URL was already found on this page.
					if _, exists := foundPublicationURLs[absolutePublicationURL]; !exists {
						// Remember the publication URL.
						foundPublicationURLs[absolutePublicationURL] = struct{}{}

						// Extract the publication title.
						publicationTitle := strings.TrimSpace(extractNodeText(currentNode))

						// Add the publication to the result.
						publications = append(publications, Publication{URL: absolutePublicationURL, Title: publicationTitle})
					}
				}
			}
		}

		// Walk through every child node.
		for childNode := currentNode.FirstChild; childNode != nil; childNode = childNode.NextSibling {
			// Recursively process the child node.
			walkHTML(childNode)
		}
	}

	// Start walking the parsed HTML document.
	walkHTML(htmlDocument)

	// Return all publications found on the page.
	return publications, nil
}

// findMeetingsOnListingPage extracts meeting page URLs from an IAASB Past Meetings page.
func (crawler *IAASBCrawler) findMeetingsOnListingPage(listingPageURL string) ([]Publication, error) {
	// Download the Past Meetings listing page.
	responseBody, err := crawler.fetchURL(listingPageURL)
	if err != nil {
		// Return the HTTP error.
		return nil, err
	}

	// Close the response body after parsing it.
	defer responseBody.Close()

	// Parse the HTML document.
	htmlDocument, err := html.Parse(responseBody)
	if err != nil {
		// Return the HTML parsing error.
		return nil, fmt.Errorf("parse Past Meetings HTML: %w", err)
	}

	// Store meetings discovered on this page.
	var meetings []Publication

	// Prevent duplicate meeting URLs on this page.
	foundMeetingURLs := make(map[string]struct{})

	// Define the recursive HTML tree walker.
	var walkHTML func(*html.Node)

	// Define the HTML tree walker.
	walkHTML = func(currentNode *html.Node) {
		// Stop when the node is nil.
		if currentNode == nil {
			// Stop processing this branch.
			return
		}

		// Inspect anchor elements.
		if currentNode.Type == html.ElementNode && currentNode.Data == "a" {
			// Convert anchor attributes into a map.
			attributes := convertAttributesToMap(currentNode.Attr)

			// Read the anchor URL.
			rawMeetingURL := strings.TrimSpace(attributes["href"])

			// Process non-empty meeting links.
			if rawMeetingURL != "" {
				// Resolve the meeting URL against the Past Meetings URL.
				absoluteMeetingURL, resolveError := resolveURL(listingPageURL, rawMeetingURL)

				// Process only valid IAASB meeting detail pages.
				if resolveError == nil && isIAASBMeetingURL(absoluteMeetingURL) {
					// Check whether this meeting URL was already found on this page.
					if _, exists := foundMeetingURLs[absoluteMeetingURL]; !exists {
						// Remember the meeting URL.
						foundMeetingURLs[absoluteMeetingURL] = struct{}{}

						// Extract the meeting title.
						meetingTitle := strings.TrimSpace(extractNodeText(currentNode))

						// Add the meeting to the result.
						meetings = append(meetings, Publication{URL: absoluteMeetingURL, Title: meetingTitle})
					}
				}
			}
		}

		// Walk through every child node.
		for childNode := currentNode.FirstChild; childNode != nil; childNode = childNode.NextSibling {
			// Recursively process the child node.
			walkHTML(childNode)
		}
	}

	// Start walking the parsed Past Meetings HTML.
	walkHTML(htmlDocument)

	// Return all meetings found on the page.
	return meetings, nil
}

// processPublication visits one IAASB page and downloads its PDFs immediately.
func (crawler *IAASBCrawler) processPublication(publication Publication) error {
	// Mark this page as visited.
	if !crawler.markPublicationPageVisited(publication.URL) {
		// Do not scrape the same page twice.
		return nil
	}

	// Log the page being visited.
	slog.Info("visiting IAASB page", "title", publication.Title, "url", publication.URL)

	// Download the page.
	responseBody, err := crawler.fetchURL(publication.URL)
	if err != nil {
		// Return the HTTP error.
		return err
	}

	// Close the page response.
	defer responseBody.Close()

	// Parse the HTML document.
	htmlDocument, err := html.Parse(responseBody)
	if err != nil {
		// Return the parsing error.
		return fmt.Errorf("parse IAASB page: %w", err)
	}

	// Find every PDF on the page.
	pdfDocuments := findPDFDocuments(htmlDocument, publication.URL)

	// Log how many PDFs were discovered.
	slog.Info("PDFs found on IAASB page", "title", publication.Title, "count", len(pdfDocuments))

	// Download every PDF immediately.
	for _, pdfDocument := range pdfDocuments {
		// Check whether this PDF URL was already processed.
		if !crawler.markPDFURLProcessed(pdfDocument.URL) {
			// Log the duplicate URL.
			slog.Info("PDF URL already processed; skipping", "url", pdfDocument.URL)
			// Continue with the next PDF.
			continue
		}

		// Download the PDF when it is not already available locally.
		if err := crawler.downloadPDFIfMissing(pdfDocument); err != nil {
			// Log the download error.
			slog.Error("failed to download PDF", "url", pdfDocument.URL, "error", err)
		}
	}

	// Return successfully.
	return nil
}

// findPDFDocuments extracts PDF resources from an IAASB page.
func findPDFDocuments(htmlDocument *html.Node, sourcePageURL string) []PDFDocument {
	// Store discovered PDF documents.
	var pdfDocuments []PDFDocument

	// Prevent duplicate PDF URLs on this page.
	foundPDFURLs := make(map[string]struct{})

	// Define the recursive HTML tree walker.
	var walkHTML func(*html.Node)

	// Define the HTML tree walker.
	walkHTML = func(currentNode *html.Node) {
		// Stop when there is no node.
		if currentNode == nil {
			// Stop processing this branch.
			return
		}

		// Inspect anchor elements.
		if currentNode.Type == html.ElementNode && currentNode.Data == "a" {
			// Convert HTML attributes into a map.
			attributes := convertAttributesToMap(currentNode.Attr)

			// Read data-file-url because IAASB commonly uses this attribute for documents.
			rawPDFURL := strings.TrimSpace(attributes["data-file-url"])

			// Fall back to href when data-file-url is unavailable.
			if rawPDFURL == "" {
				// Read the href attribute.
				rawPDFURL = strings.TrimSpace(attributes["href"])
			}

			// Ignore empty links.
			if rawPDFURL != "" {
				// Resolve the document URL against the source page.
				absolutePDFURL, resolveError := resolveURL(sourcePageURL, rawPDFURL)

				// Process URLs that look like PDFs or IAASB file resources.
				if resolveError == nil && isPDFURL(absolutePDFURL) {
					// Check whether this PDF URL already appeared on this page.
					if _, exists := foundPDFURLs[absolutePDFURL]; !exists {
						// Remember the PDF URL.
						foundPDFURLs[absolutePDFURL] = struct{}{}

						// Create a useful local filename.
						fileName := createPDFFilename(absolutePDFURL, extractNodeText(currentNode))

						// Add the PDF to the result.
						pdfDocuments = append(pdfDocuments, PDFDocument{URL: absolutePDFURL, SourcePageURL: sourcePageURL, FileName: fileName})
					}
				}
			}
		}

		// Walk through every child node.
		for childNode := currentNode.FirstChild; childNode != nil; childNode = childNode.NextSibling {
			// Recursively process the child node.
			walkHTML(childNode)
		}
	}

	// Start walking the parsed HTML document.
	walkHTML(htmlDocument)

	// Return all discovered PDFs.
	return pdfDocuments
}

// downloadPDFIfMissing downloads a PDF when an identical local PDF does not exist.
func (crawler *IAASBCrawler) downloadPDFIfMissing(pdfDocument PDFDocument) error {
	// Sanitize the requested local filename.
	fileName := sanitizeFilename(pdfDocument.FileName)

	// Build the expected local PDF path.
	localFilePath := filepath.Join(crawler.OutputDirectory, fileName)

	// Check whether the exact filename already exists.
	if _, err := os.Stat(localFilePath); err == nil {
		// Log that the local file already exists.
		slog.Info("PDF already exists locally; skipping download", "file", localFilePath)
		// Do not download it again.
		return nil
	}

	// Wait before making the PDF request.
	time.Sleep(requestDelay)

	// Log the PDF download.
	slog.Info("downloading PDF", "url", pdfDocument.URL, "file", localFilePath)

	// Download the PDF.
	responseBody, err := crawler.fetchURL(pdfDocument.URL)
	if err != nil {
		// Return the HTTP error.
		return err
	}

	// Close the response body.
	defer responseBody.Close()

	// Create a temporary file in the PDF directory.
	temporaryFile, err := os.CreateTemp(crawler.OutputDirectory, ".iaasb-download-*.tmp")
	if err != nil {
		// Return the temporary file creation error.
		return fmt.Errorf("create temporary PDF: %w", err)
	}

	// Store the temporary file path.
	temporaryFilePath := temporaryFile.Name()

	// Remove the temporary file when this function exits.
	defer os.Remove(temporaryFilePath)

	// Create a SHA-256 hash calculator.
	fileHash := sha256.New()

	// Copy the PDF while calculating its SHA-256 hash.
	if _, err := io.Copy(io.MultiWriter(temporaryFile, fileHash), responseBody); err != nil {
		// Close the temporary file after the copy fails.
		_ = temporaryFile.Close()
		// Return the download error.
		return fmt.Errorf("download PDF: %w", err)
	}

	// Close the temporary file.
	if err := temporaryFile.Close(); err != nil {
		// Return the close error.
		return fmt.Errorf("close temporary PDF: %w", err)
	}

	// Convert the hash into a hexadecimal string.
	pdfHash := hex.EncodeToString(fileHash.Sum(nil))

	// Search the local PDF directory for identical content.
	existingFilePath, exists, err := crawler.findPDFByHash(pdfHash)
	if err != nil {
		// Return the duplicate detection error.
		return fmt.Errorf("check duplicate PDF: %w", err)
	}

	// Skip the new file when identical content already exists.
	if exists {
		// Log the duplicate PDF.
		slog.Info("identical PDF already exists; skipping", "existing_file", existingFilePath, "sha256", pdfHash, "url", pdfDocument.URL)
		// Do not save the duplicate.
		return nil
	}

	// Create a unique filename when another file has the same name.
	finalFileName := crawler.createUniqueFilename(fileName, pdfHash)

	// Build the final output path.
	finalFilePath := filepath.Join(crawler.OutputDirectory, finalFileName)

	// Move the temporary file to the final location.
	if err := os.Rename(temporaryFilePath, finalFilePath); err != nil {
		// Return the filesystem error.
		return fmt.Errorf("save PDF: %w", err)
	}

	// Log the successful PDF download.
	slog.Info("PDF downloaded", "file", finalFilePath, "sha256", pdfHash)

	// Return successfully.
	return nil
}

// findPDFByHash checks existing local PDFs for identical content.
func (crawler *IAASBCrawler) findPDFByHash(targetHash string) (string, bool, error) {
	// Read all files in the PDF directory.
	entries, err := os.ReadDir(crawler.OutputDirectory)
	if err != nil {
		// Return the directory error.
		return "", false, err
	}

	// Check every directory entry.
	for _, entry := range entries {
		// Ignore directories.
		if entry.IsDir() {
			// Continue with the next entry.
			continue
		}

		// Ignore files that are not PDFs.
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".pdf") {
			// Continue with the next entry.
			continue
		}

		// Build the full path to the existing PDF.
		existingFilePath := filepath.Join(crawler.OutputDirectory, entry.Name())

		// Open the existing PDF.
		existingFile, err := os.Open(existingFilePath)
		if err != nil {
			// Ignore files that cannot be opened.
			continue
		}

		// Create a SHA-256 hash calculator.
		existingFileHash := sha256.New()

		// Calculate the existing PDF hash.
		_, copyError := io.Copy(existingFileHash, existingFile)

		// Close the existing PDF.
		closeError := existingFile.Close()

		// Ignore unreadable files.
		if copyError != nil || closeError != nil {
			// Continue with the next file.
			continue
		}

		// Convert the existing hash into hexadecimal.
		existingHash := hex.EncodeToString(existingFileHash.Sum(nil))

		// Compare the existing hash with the target hash.
		if existingHash == targetHash {
			// Return the duplicate PDF path.
			return existingFilePath, true, nil
		}
	}

	// No identical PDF was found.
	return "", false, nil
}

// createUniqueFilename creates a filename that does not overwrite another PDF.
func (crawler *IAASBCrawler) createUniqueFilename(fileName string, pdfHash string) string {
	// Build the initial output path.
	outputPath := filepath.Join(crawler.OutputDirectory, fileName)

	// Return the filename when it does not exist.
	if _, err := os.Stat(outputPath); errors.Is(err, os.ErrNotExist) {
		// Return the available filename.
		return fileName
	}

	// Extract the filename extension.
	fileExtension := filepath.Ext(fileName)

	// Remove the extension from the filename.
	baseName := strings.TrimSuffix(fileName, fileExtension)

	// Append part of the SHA-256 hash to avoid overwriting the existing file.
	return fmt.Sprintf("%s-%s%s", baseName, pdfHash[:12], fileExtension)
}

// fetchURL performs an HTTP GET request.
func (crawler *IAASBCrawler) fetchURL(rawURL string) (io.ReadCloser, error) {
	// Create the HTTP request.
	request, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		// Return the request creation error.
		return nil, fmt.Errorf("create HTTP request: %w", err)
	}

	// Set the crawler User-Agent.
	request.Header.Set("User-Agent", crawlerUserAgent)

	// Tell the server which content types are accepted.
	request.Header.Set("Accept", "text/html,application/pdf,*/*")

	// Execute the HTTP request.
	response, err := httpClient.Do(request)
	if err != nil {
		// Return the HTTP error.
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}

	// Check whether the server returned a successful response.
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		// Close the unsuccessful response.
		response.Body.Close()
		// Return the HTTP status error.
		return nil, fmt.Errorf("unexpected HTTP status: %s", response.Status)
	}

	// Return the successful response body.
	return response.Body, nil
}

// markPublicationPageVisited marks an IAASB page as visited.
func (crawler *IAASBCrawler) markPublicationPageVisited(publicationURL string) bool {
	// Lock the crawler state.
	crawler.Mutex.Lock()

	// Unlock the crawler state before returning.
	defer crawler.Mutex.Unlock()

	// Check whether the page has already been visited.
	if _, exists := crawler.VisitedPublicationPages[publicationURL]; exists {
		// Report that the page was already visited.
		return false
	}

	// Remember the page URL.
	crawler.VisitedPublicationPages[publicationURL] = struct{}{}

	// Report that this is a new page.
	return true
}

// markPDFURLProcessed marks a PDF URL as processed.
func (crawler *IAASBCrawler) markPDFURLProcessed(pdfURL string) bool {
	// Lock the crawler state.
	crawler.Mutex.Lock()

	// Unlock the crawler state before returning.
	defer crawler.Mutex.Unlock()

	// Check whether this PDF URL has already been processed.
	if _, exists := crawler.ProcessedPDFURLs[pdfURL]; exists {
		// Report that the PDF URL was already processed.
		return false
	}

	// Remember the PDF URL.
	crawler.ProcessedPDFURLs[pdfURL] = struct{}{}

	// Report that this is a new PDF URL.
	return true
}

// convertAttributesToMap converts HTML attributes into a string map.
func convertAttributesToMap(attributes []html.Attribute) map[string]string {
	// Create the result map.
	attributeMap := make(map[string]string, len(attributes))

	// Process every HTML attribute.
	for _, attribute := range attributes {
		// Store the attribute using a lowercase name.
		attributeMap[strings.ToLower(attribute.Key)] = attribute.Val
	}

	// Return the attribute map.
	return attributeMap
}

// extractNodeText extracts all text contained inside an HTML node.
func extractNodeText(node *html.Node) string {
	// Return an empty string for a nil node.
	if node == nil {
		// Return an empty string.
		return ""
	}

	// Return the text when this is a text node.
	if node.Type == html.TextNode {
		// Return the node text.
		return node.Data
	}

	// Create a string builder.
	var textBuilder strings.Builder

	// Process every child node.
	for childNode := node.FirstChild; childNode != nil; childNode = childNode.NextSibling {
		// Append the child's text.
		textBuilder.WriteString(extractNodeText(childNode))
	}

	// Return the complete text.
	return textBuilder.String()
}

// resolveURL converts a relative URL into an absolute URL.
func resolveURL(baseURL string, rawURL string) (string, error) {
	// Reject empty URLs.
	if strings.TrimSpace(rawURL) == "" {
		// Return an empty URL error.
		return "", errors.New("empty URL")
	}

	// Parse the base URL.
	parsedBaseURL, err := url.Parse(baseURL)
	if err != nil {
		// Return the parsing error.
		return "", err
	}

	// Parse the target URL.
	parsedTargetURL, err := url.Parse(rawURL)
	if err != nil {
		// Return the parsing error.
		return "", err
	}

	// Resolve the target URL against the base URL.
	return parsedBaseURL.ResolveReference(parsedTargetURL).String(), nil
}

// isIAASBPublicationURL checks whether a URL is an IAASB publication URL.
func isIAASBPublicationURL(rawURL string) bool {
	// Parse the URL.
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		// Reject invalid URLs.
		return false
	}

	// Only accept the official IAASB hostname.
	if !strings.EqualFold(parsedURL.Hostname(), "www.iaasb.org") {
		// Reject other domains.
		return false
	}

	// Only accept IAASB publication paths.
	return strings.HasPrefix(parsedURL.Path, "/publications/")
}

// isIAASBMeetingURL checks whether a URL is an IAASB meeting detail page.
func isIAASBMeetingURL(rawURL string) bool {
	// Parse the URL.
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		// Reject invalid URLs.
		return false
	}

	// Only accept the official IAASB hostname.
	if !strings.EqualFold(parsedURL.Hostname(), "www.iaasb.org") {
		// Reject other domains.
		return false
	}

	// Read the URL path.
	meetingPath := strings.TrimSuffix(parsedURL.Path, "/")

	// Reject the Past Meetings listing itself.
	if meetingPath == "/meetings/past-meetings" {
		// Reject the listing page.
		return false
	}

	// Accept IAASB meeting detail pages.
	return strings.HasPrefix(meetingPath, "/meetings/") && strings.Count(strings.TrimPrefix(meetingPath, "/meetings/"), "/") == 0
}

// isPDFURL checks whether a URL represents an IAASB PDF resource.
func isPDFURL(rawURL string) bool {
	// Parse the URL.
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		// Reject invalid URLs.
		return false
	}

	// Convert the URL path to lowercase.
	lowercasePath := strings.ToLower(parsedURL.Path)

	// Accept normal URLs ending with PDF.
	if strings.HasSuffix(lowercasePath, ".pdf") {
		// Report that this is a PDF.
		return true
	}

	// Accept IAASB Flysystem URLs.
	if strings.Contains(lowercasePath, "/_flysystem/") {
		// Report that this is an IAASB file.
		return true
	}

	// Accept Drupal public file URLs.
	if strings.Contains(lowercasePath, "/sites/default/files/") {
		// Report that this is an IAASB file.
		return true
	}

	// Reject all other URLs.
	return false
}

// createPDFFilename creates a useful local filename from a PDF URL.
func createPDFFilename(rawPDFURL string, linkText string) string {
	// Parse the PDF URL.
	parsedURL, err := url.Parse(rawPDFURL)
	if err != nil {
		// Return a fallback filename.
		return "iaasb-document.pdf"
	}

	// Extract the final filename from the URL path.
	fileName := filepath.Base(parsedURL.Path)

	// Decode URL-encoded filename characters.
	if decodedFileName, err := url.PathUnescape(fileName); err == nil {
		// Use the decoded filename.
		fileName = decodedFileName
	}

	// Use the link text when no filename exists.
	if fileName == "" || fileName == "." || fileName == "/" {
		// Generate the filename from the link text.
		fileName = sanitizeFilename(linkText)

		// Add the PDF extension when necessary.
		if !strings.HasSuffix(strings.ToLower(fileName), ".pdf") {
			// Add the PDF extension.
			fileName += ".pdf"
		}
	}

	// Return a safe filename.
	return sanitizeFilename(fileName)
}

// sanitizeFilename makes a filename safe for the local filesystem.
func sanitizeFilename(fileName string) string {
	// Replace unsafe characters.
	fileName = invalidFilenameCharacters.Replace(fileName)

	// Remove leading and trailing whitespace.
	fileName = strings.TrimSpace(fileName)

	// Use a fallback when the filename is empty.
	if fileName == "" {
		// Set the fallback filename.
		fileName = "iaasb-document.pdf"
	}

	// Return the safe filename.
	return fileName
}
