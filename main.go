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

	// Import sync for protecting shared crawler data.
	"sync"

	// Import time to add a delay between requests.
	"time"

	// Import the HTML parser used to inspect IAASB pages.
	"golang.org/x/net/html"
)

// standardsPageURL is the IAASB standards listing URL.
const standardsPageURL = "https://www.iaasb.org/support-resources?language=399&page=%d"

// pdfDirectory is the directory where PDFs will be saved.
const pdfDirectory = "PDFs"

// crawlerUserAgent identifies this program to the IAASB server.
const crawlerUserAgent = "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

// maximumWorkers controls how many publication pages can be processed concurrently.
const maximumWorkers = 5

// requestDelay controls the delay between requests.
const requestDelay = 750 * time.Millisecond

// httpClient is the shared HTTP client used by the crawler.
var httpClient = &http.Client{
	// Set a reasonable timeout for each HTTP request.
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

// Publication represents one IAASB publication page.
type Publication struct {
	// URL contains the publication page URL.
	URL string

	// Title contains the publication title.
	Title string
}

// PDFDocument represents one PDF discovered on a publication page.
type PDFDocument struct {
	// URL contains the PDF download URL.
	URL string

	// SourcePageURL contains the publication page where the PDF was found.
	SourcePageURL string

	// FileName contains the desired local filename.
	FileName string
}

// IAASBCrawler contains the crawler's in-memory state.
type IAASBCrawler struct {
	// OutputDirectory contains the directory where PDFs are stored.
	OutputDirectory string

	// VisitedPublicationPages contains publication URLs already scraped.
	VisitedPublicationPages map[string]struct{}

	// ProcessedPDFURLs contains PDF URLs already processed.
	ProcessedPDFURLs map[string]struct{}

	// Mutex protects the crawler's shared maps.
	Mutex sync.Mutex
}

// createCrawler creates a new IAASB crawler.
func createCrawler(outputDirectory string) *IAASBCrawler {
	// Return a crawler with initialized in-memory state.
	return &IAASBCrawler{
		// Set the PDF output directory.
		OutputDirectory: outputDirectory,

		// Create the set used to track scraped publication pages.
		VisitedPublicationPages: make(map[string]struct{}),

		// Create the set used to track processed PDF URLs.
		ProcessedPDFURLs: make(map[string]struct{}),
	}
}

// main starts the IAASB crawler.
func main() {
	// Create the PDFs directory if it does not already exist.
	if err := os.MkdirAll(pdfDirectory, 0o755); err != nil {
		// Log the directory creation error.
		slog.Error("unable to create PDF directory", "error", err)

		// Exit with a failure status.
		os.Exit(1)
	}

	// Create the IAASB crawler.
	crawler := createCrawler(pdfDirectory)

	// Start crawling IAASB.
	if err := crawler.crawlStandardsPages(); err != nil {
		// Log the crawler error.
		slog.Error("crawler failed", "error", err)

		// Exit with a failure status.
		os.Exit(1)
	}

	// Report successful completion.
	slog.Info("IAASB crawler completed successfully")
}

// crawlStandardsPages visits IAASB listing pages until no new publications are found.
func (crawler *IAASBCrawler) crawlStandardsPages() error {
	// Keep track of every unique publication URL found during this run.
	knownPublicationPages := make(map[string]struct{})

	// Start crawling at page zero.
	pageNumber := 0

	// Continue until IAASB produces no new publication URLs.
	for {
		// Build the current standards listing URL.
		currentPageURL := fmt.Sprintf(standardsPageURL, pageNumber)

		// Log the page being visited.
		slog.Info(
			"visiting standards listing page",
			"page", pageNumber,
			"url", currentPageURL,
		)

		// Find all publication pages on the current listing page.
		publications, err := crawler.findPublicationsOnListingPage(currentPageURL)
		if err != nil {
			// Return the listing page error.
			return fmt.Errorf(
				"find publications on page %d: %w",
				pageNumber,
				err,
			)
		}

		// Count how many new publication URLs were found.
		newPublicationCount := 0

		// Process publications in the order they were discovered.
		for _, publication := range publications {
			// Check whether this publication URL was already discovered.
			if _, exists := knownPublicationPages[publication.URL]; exists {
				// Skip the duplicate publication URL.
				continue
			}

			// Remember this publication URL.
			knownPublicationPages[publication.URL] = struct{}{}

			// Increment the new publication counter.
			newPublicationCount++

			// Immediately visit the publication and download its PDFs.
			if err := crawler.processPublication(publication); err != nil {
				// Log the error but continue with the remaining publications.
				slog.Error(
					"failed to process publication",
					"title", publication.Title,
					"url", publication.URL,
					"error", err,
				)
			}
		}

		// Log the results for the current listing page.
		slog.Info(
			"standards listing page completed",
			"page", pageNumber,
			"publications_on_page", len(publications),
			"new_publications", newPublicationCount,
			"total_unique_publications", len(knownPublicationPages),
		)

		// Stop when this page contains no new publication URLs.
		if newPublicationCount == 0 {
			// Log the stopping condition.
			slog.Info(
				"no new publication URLs found; stopping",
				"page", pageNumber,
			)

			// Stop crawling listing pages.
			break
		}

		// Move to the next IAASB listing page.
		pageNumber++

		// Wait before requesting the next listing page.
		time.Sleep(requestDelay)
	}

	// Return successfully.
	return nil
}

// findPublicationsOnListingPage extracts publication URLs from one listing page.
func (crawler *IAASBCrawler) findPublicationsOnListingPage(
	listingPageURL string,
) ([]Publication, error) {
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
			return
		}

		// Check whether the current node is an anchor element.
		if currentNode.Type == html.ElementNode &&
			currentNode.Data == "a" {
			// Convert the anchor attributes into a map.
			attributes := convertAttributesToMap(currentNode.Attr)

			// Read the anchor's CSS class.
			anchorClass := attributes["class"]

			// Look specifically for IAASB publication card links.
			if strings.Contains(anchorClass, "card__link-wrap") {
				// Read the publication URL.
				rawPublicationURL := strings.TrimSpace(
					attributes["href"],
				)

				// Convert the relative URL into an absolute URL.
				absolutePublicationURL, resolveError := resolveURL(
					listingPageURL,
					rawPublicationURL,
				)

				// Process only valid IAASB publication URLs.
				if resolveError == nil &&
					isIAASBPublicationURL(absolutePublicationURL) {
					// Check whether the URL was already found on this page.
					if _, exists := foundPublicationURLs[absolutePublicationURL]; !exists {
						// Remember this publication URL.
						foundPublicationURLs[absolutePublicationURL] = struct{}{}

						// Extract the publication title.
						publicationTitle := strings.TrimSpace(
							extractNodeText(currentNode),
						)

						// Add the publication to the result.
						publications = append(
							publications,
							Publication{
								// Store the publication URL.
								URL: absolutePublicationURL,

								// Store the publication title.
								Title: publicationTitle,
							},
						)
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

// processPublication visits one publication page and downloads its PDFs immediately.
func (crawler *IAASBCrawler) processPublication(
	publication Publication,
) error {
	// Mark this publication page as visited.
	if !crawler.markPublicationPageVisited(publication.URL) {
		// Do not scrape the same page twice.
		return nil
	}

	// Log the publication being visited.
	slog.Info(
		"visiting publication page",
		"title", publication.Title,
		"url", publication.URL,
	)

	// Download the publication page.
	responseBody, err := crawler.fetchURL(publication.URL)
	if err != nil {
		// Return the HTTP error.
		return err
	}

	// Close the publication page response.
	defer responseBody.Close()

	// Parse the publication HTML.
	htmlDocument, err := html.Parse(responseBody)
	if err != nil {
		// Return the parsing error.
		return fmt.Errorf(
			"parse publication page: %w",
			err,
		)
	}

	// Find all PDFs on this publication page.
	pdfDocuments := findPDFDocuments(
		htmlDocument,
		publication.URL,
	)

	// Log how many PDFs were found.
	slog.Info(
		"PDFs found on publication page",
		"title", publication.Title,
		"count", len(pdfDocuments),
	)

	// Download each PDF immediately.
	for _, pdfDocument := range pdfDocuments {
		// Check whether this PDF URL has already been processed.
		if !crawler.markPDFURLProcessed(pdfDocument.URL) {
			// Skip the already-processed PDF URL.
			slog.Info(
				"PDF URL already processed; skipping",
				"url", pdfDocument.URL,
			)

			// Continue with the next PDF.
			continue
		}

		// Download the PDF if it is not already stored locally.
		if err := crawler.downloadPDFIfMissing(pdfDocument); err != nil {
			// Log the PDF error.
			slog.Error(
				"failed to download PDF",
				"url", pdfDocument.URL,
				"error", err,
			)
		}
	}

	// Return successfully.
	return nil
}

// findPDFDocuments extracts PDF resources from a publication page.
func findPDFDocuments(
	htmlDocument *html.Node,
	sourcePageURL string,
) []PDFDocument {
	// Store discovered PDF documents.
	var pdfDocuments []PDFDocument

	// Prevent duplicate PDF URLs on this page.
	foundPDFURLs := make(map[string]struct{})

	// Define the recursive HTML walker.
	var walkHTML func(*html.Node)

	// Define the HTML walker.
	walkHTML = func(currentNode *html.Node) {
		// Stop when there is no node.
		if currentNode == nil {
			return
		}

		// Only inspect anchor elements.
		if currentNode.Type == html.ElementNode &&
			currentNode.Data == "a" {
			// Convert HTML attributes into a map.
			attributes := convertAttributesToMap(currentNode.Attr)

			// IAASB commonly stores PDF URLs in data-file-url.
			rawPDFURL := strings.TrimSpace(
				attributes["data-file-url"],
			)

			// Fall back to href when data-file-url is unavailable.
			if rawPDFURL == "" {
				// Read the href attribute.
				rawPDFURL = strings.TrimSpace(
					attributes["href"],
				)
			}

			// Resolve the PDF URL against the publication page.
			absolutePDFURL, resolveError := resolveURL(
				sourcePageURL,
				rawPDFURL,
			)

			// Check whether the URL looks like an IAASB PDF resource.
			if resolveError == nil &&
				isPDFURL(absolutePDFURL) {
				// Check whether this PDF URL already appeared on this page.
				if _, exists := foundPDFURLs[absolutePDFURL]; !exists {
					// Remember this PDF URL.
					foundPDFURLs[absolutePDFURL] = struct{}{}

					// Create the local PDF filename.
					fileName := createPDFFilename(
						absolutePDFURL,
						extractNodeText(currentNode),
					)

					// Add the PDF to the result.
					pdfDocuments = append(
						pdfDocuments,
						PDFDocument{
							// Store the PDF URL.
							URL: absolutePDFURL,

							// Store the source page URL.
							SourcePageURL: sourcePageURL,

							// Store the local filename.
							FileName: fileName,
						},
					)
				}
			}
		}

		// Walk through every child node.
		for childNode := currentNode.FirstChild; childNode != nil; childNode = childNode.NextSibling {
			// Recursively process the child node.
			walkHTML(childNode)
		}
	}

	// Start walking the HTML document.
	walkHTML(htmlDocument)

	// Return all discovered PDFs.
	return pdfDocuments
}

// downloadPDFIfMissing downloads a PDF when an identical local PDF does not exist.
func (crawler *IAASBCrawler) downloadPDFIfMissing(
	pdfDocument PDFDocument,
) error {
	// Sanitize the requested local filename.
	fileName := sanitizeFilename(pdfDocument.FileName)

	// Build the expected local PDF path.
	localFilePath := filepath.Join(
		crawler.OutputDirectory,
		fileName,
	)

	// Check whether the exact filename already exists.
	if _, err := os.Stat(localFilePath); err == nil {
		// Log that the local file already exists.
		slog.Info(
			"PDF already exists locally; skipping download",
			"file", localFilePath,
		)

		// Do not download it again.
		return nil
	}

	// Acquire the download semaphore.
	crawler.Mutex.Lock()

	// Release the mutex after checking the local file.
	crawler.Mutex.Unlock()

	// Wait before making the PDF request.
	time.Sleep(requestDelay)

	// Log the PDF download.
	slog.Info(
		"downloading PDF",
		"url", pdfDocument.URL,
		"file", localFilePath,
	)

	// Download the PDF.
	responseBody, err := crawler.fetchURL(pdfDocument.URL)
	if err != nil {
		// Return the HTTP error.
		return err
	}

	// Close the response body.
	defer responseBody.Close()

	// Create a temporary file in the PDF directory.
	temporaryFile, err := os.CreateTemp(
		crawler.OutputDirectory,
		".iaasb-download-*.tmp",
	)
	if err != nil {
		// Return the temporary file creation error.
		return fmt.Errorf(
			"create temporary PDF: %w",
			err,
		)
	}

	// Store the temporary file path.
	temporaryFilePath := temporaryFile.Name()

	// Remove the temporary file when this function exits.
	defer os.Remove(temporaryFilePath)

	// Create a SHA-256 hash calculator.
	fileHash := sha256.New()

	// Copy the PDF into the temporary file while calculating its hash.
	if _, err := io.Copy(
		io.MultiWriter(temporaryFile, fileHash),
		responseBody,
	); err != nil {
		// Close the temporary file after the copy fails.
		_ = temporaryFile.Close()

		// Return the copy error.
		return fmt.Errorf(
			"download PDF: %w",
			err,
		)
	}

	// Close the temporary file.
	if err := temporaryFile.Close(); err != nil {
		// Return the close error.
		return fmt.Errorf(
			"close temporary PDF: %w",
			err,
		)
	}

	// Convert the hash into a hexadecimal string.
	pdfHash := hex.EncodeToString(
		fileHash.Sum(nil),
	)

	// Search the local PDFs directory for identical content.
	existingFilePath, exists, err := crawler.findPDFByHash(
		pdfHash,
	)
	if err != nil {
		// Return the duplicate detection error.
		return fmt.Errorf(
			"check duplicate PDF: %w",
			err,
		)
	}

	// Skip the new file when identical content already exists.
	if exists {
		// Log the duplicate PDF.
		slog.Info(
			"identical PDF already exists; skipping",
			"existing_file", existingFilePath,
			"sha256", pdfHash,
			"url", pdfDocument.URL,
		)

		// Do not save the duplicate.
		return nil
	}

	// Make sure the desired filename does not overwrite another file.
	finalFileName := crawler.createUniqueFilename(
		fileName,
		pdfHash,
	)

	// Build the final output path.
	finalFilePath := filepath.Join(
		crawler.OutputDirectory,
		finalFileName,
	)

	// Move the temporary file to its final location.
	if err := os.Rename(
		temporaryFilePath,
		finalFilePath,
	); err != nil {
		// Return the filesystem error.
		return fmt.Errorf(
			"save PDF: %w",
			err,
		)
	}

	// Log the successful download.
	slog.Info(
		"PDF downloaded",
		"file", finalFilePath,
		"sha256", pdfHash,
	)

	// Return successfully.
	return nil
}

// findPDFByHash checks existing local PDFs for identical content.
func (crawler *IAASBCrawler) findPDFByHash(
	targetHash string,
) (string, bool, error) {
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
			continue
		}

		// Ignore files that are not PDFs.
		if !strings.EqualFold(
			filepath.Ext(entry.Name()),
			".pdf",
		) {
			continue
		}

		// Build the full path to the existing PDF.
		existingFilePath := filepath.Join(
			crawler.OutputDirectory,
			entry.Name(),
		)

		// Open the existing PDF.
		existingFile, err := os.Open(existingFilePath)
		if err != nil {
			// Ignore files that cannot be opened.
			continue
		}

		// Create a SHA-256 hash calculator.
		existingFileHash := sha256.New()

		// Calculate the existing PDF hash.
		_, copyError := io.Copy(
			existingFileHash,
			existingFile,
		)

		// Close the existing PDF.
		closeError := existingFile.Close()

		// Ignore unreadable files.
		if copyError != nil || closeError != nil {
			continue
		}

		// Convert the existing hash to hexadecimal.
		existingHash := hex.EncodeToString(
			existingFileHash.Sum(nil),
		)

		// Compare the existing hash with the downloaded PDF hash.
		if existingHash == targetHash {
			// Return the duplicate PDF path.
			return existingFilePath, true, nil
		}
	}

	// No identical PDF was found.
	return "", false, nil
}

// createUniqueFilename creates a filename that will not overwrite another PDF.
func (crawler *IAASBCrawler) createUniqueFilename(
	fileName string,
	pdfHash string,
) string {
	// Build the initial output path.
	outputPath := filepath.Join(
		crawler.OutputDirectory,
		fileName,
	)

	// Return the filename when it does not exist.
	if _, err := os.Stat(outputPath); errors.Is(
		err,
		os.ErrNotExist,
	) {
		// Return the available filename.
		return fileName
	}

	// Extract the filename extension.
	fileExtension := filepath.Ext(fileName)

	// Remove the extension from the filename.
	baseName := strings.TrimSuffix(
		fileName,
		fileExtension,
	)

	// Append part of the SHA-256 hash to the filename.
	return fmt.Sprintf(
		"%s-%s%s",
		baseName,
		pdfHash[:12],
		fileExtension,
	)
}

// fetchURL performs an HTTP GET request.
func (crawler *IAASBCrawler) fetchURL(
	rawURL string,
) (io.ReadCloser, error) {
	// Create the HTTP request.
	request, err := http.NewRequest(
		http.MethodGet,
		rawURL,
		nil,
	)
	if err != nil {
		// Return the request creation error.
		return nil, fmt.Errorf(
			"create HTTP request: %w",
			err,
		)
	}

	// Set the crawler User-Agent.
	request.Header.Set(
		"User-Agent",
		crawlerUserAgent,
	)

	// Tell the server which content types are accepted.
	request.Header.Set(
		"Accept",
		"text/html,application/pdf,*/*",
	)

	// Execute the HTTP request.
	response, err := httpClient.Do(request)
	if err != nil {
		// Return the HTTP error.
		return nil, fmt.Errorf(
			"HTTP request failed: %w",
			err,
		)
	}

	// Check whether the server returned a successful response.
	if response.StatusCode < http.StatusOK ||
		response.StatusCode >= http.StatusMultipleChoices {
		// Close the unsuccessful response.
		response.Body.Close()

		// Return the HTTP status error.
		return nil, fmt.Errorf(
			"unexpected HTTP status: %s",
			response.Status,
		)
	}

	// Return the successful response body.
	return response.Body, nil
}

// markPublicationPageVisited marks a publication page as visited.
func (crawler *IAASBCrawler) markPublicationPageVisited(
	publicationURL string,
) bool {
	// Lock the crawler state.
	crawler.Mutex.Lock()

	// Unlock the crawler state before returning.
	defer crawler.Mutex.Unlock()

	// Check whether this page has already been visited.
	if _, exists := crawler.VisitedPublicationPages[publicationURL]; exists {
		// Report that the page was already visited.
		return false
	}

	// Remember the publication URL.
	crawler.VisitedPublicationPages[publicationURL] = struct{}{}

	// Report that this is a new publication page.
	return true
}

// markPDFURLProcessed marks a PDF URL as processed.
func (crawler *IAASBCrawler) markPDFURLProcessed(
	pdfURL string,
) bool {
	// Lock the crawler state.
	crawler.Mutex.Lock()

	// Unlock the crawler state before returning.
	defer crawler.Mutex.Unlock()

	// Check whether this PDF URL was already processed.
	if _, exists := crawler.ProcessedPDFURLs[pdfURL]; exists {
		// Report that this PDF URL was already processed.
		return false
	}

	// Remember the PDF URL.
	crawler.ProcessedPDFURLs[pdfURL] = struct{}{}

	// Report that this is a new PDF URL.
	return true
}

// convertAttributesToMap converts HTML attributes into a string map.
func convertAttributesToMap(
	attributes []html.Attribute,
) map[string]string {
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
		return ""
	}

	// Return the text when this is a text node.
	if node.Type == html.TextNode {
		return node.Data
	}

	// Create a string builder.
	var textBuilder strings.Builder

	// Process every child node.
	for childNode := node.FirstChild; childNode != nil; childNode = childNode.NextSibling {
		// Append the child's text.
		textBuilder.WriteString(
			extractNodeText(childNode),
		)
	}

	// Return the complete text.
	return textBuilder.String()
}

// resolveURL converts a relative URL into an absolute URL.
func resolveURL(
	baseURL string,
	rawURL string,
) (string, error) {
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
	return parsedBaseURL.ResolveReference(
		parsedTargetURL,
	).String(), nil
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
	if !strings.EqualFold(
		parsedURL.Hostname(),
		"www.iaasb.org",
	) {
		// Reject other domains.
		return false
	}

	// Only accept IAASB publication paths.
	return strings.HasPrefix(
		parsedURL.Path,
		"/publications/",
	)
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
	lowercasePath := strings.ToLower(
		parsedURL.Path,
	)

	// Accept normal URLs ending with .pdf.
	if strings.HasSuffix(
		lowercasePath,
		".pdf",
	) {
		// This is a PDF.
		return true
	}

	// Accept IAASB Flysystem URLs.
	if strings.Contains(
		lowercasePath,
		"/_flysystem/",
	) {
		// This is an IAASB file.
		return true
	}

	// Accept Drupal public file URLs.
	if strings.Contains(
		lowercasePath,
		"/sites/default/files/",
	) {
		// This is an IAASB file.
		return true
	}

	// Reject all other URLs.
	return false
}

// createPDFFilename creates a useful local filename from a PDF URL.
func createPDFFilename(
	rawPDFURL string,
	linkText string,
) string {
	// Parse the PDF URL.
	parsedURL, err := url.Parse(rawPDFURL)
	if err != nil {
		// Return a fallback filename.
		return "iaasb-document.pdf"
	}

	// Extract the final filename from the URL path.
	fileName := filepath.Base(
		parsedURL.Path,
	)

	// Decode URL-encoded filename characters.
	if decodedFileName, err := url.PathUnescape(
		fileName,
	); err == nil {
		// Use the decoded filename.
		fileName = decodedFileName
	}

	// Use the link text when no filename exists.
	if fileName == "" ||
		fileName == "." ||
		fileName == "/" {
		// Generate the filename from the link text.
		fileName = sanitizeFilename(linkText)

		// Add the PDF extension when necessary.
		if !strings.HasSuffix(
			strings.ToLower(fileName),
			".pdf",
		) {
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
	fileName = invalidFilenameCharacters.Replace(
		fileName,
	)

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
