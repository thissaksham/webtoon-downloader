package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/anaskhan96/soup"
	"github.com/cheggaaa/pb/v3"
	"github.com/signintech/gopdf"
)

type MotiontoonJson struct {
	Assets struct {
		Image map[string]string `json:"image"`
	} `json:"assets"`
}

type EpisodeBatch struct {
	imgLinks []string
	minEp    int
	maxEp    int
}

type ComicFile interface {
	addImage([]byte, string) error
	save(outFile string) error
}

type PDFComicFile struct {
	pdf *gopdf.GoPdf
}

var _ ComicFile = &PDFComicFile{}

func newPDFComicFile() *PDFComicFile {
	pdf := gopdf.GoPdf{}
	pdf.Start(gopdf.Config{Unit: gopdf.UnitPT, PageSize: *gopdf.PageSizeA4})
	return &PDFComicFile{pdf: &pdf}
}

func (c *PDFComicFile) addImage(img []byte, ext string) error {
	holder, err := gopdf.ImageHolderByBytes(img)
	if err != nil {
		return err
	}

	d, _, err := image.DecodeConfig(bytes.NewReader(img))
	if err != nil {
		return err
	}

	c.pdf.AddPageWithOption(gopdf.PageOption{PageSize: &gopdf.Rect{
		W: float64(d.Width)*72/128 - 1,
		H: float64(d.Height)*72/128 - 1,
	}})
	return c.pdf.ImageByHolder(holder, 0, 0, nil)
}

func (c *PDFComicFile) save(outputPath string) error {
	return c.pdf.WritePdf(outputPath)
}

type CBZComicFile struct {
	zipWriter *zip.Writer
	buffer    *bytes.Buffer
	numFiles  int
}

var _ ComicFile = &CBZComicFile{}

func newCBZComicFile() (*CBZComicFile, error) {
	buffer := new(bytes.Buffer)
	zipWriter := zip.NewWriter(buffer)
	return &CBZComicFile{zipWriter: zipWriter, buffer: buffer, numFiles: 0}, nil
}

func (c *CBZComicFile) addImage(img []byte, ext string) error {
	f, err := c.zipWriter.Create(fmt.Sprintf("%010d.%s", c.numFiles, ext))
	if err != nil {
		return err
	}
	_, err = f.Write(img)
	if err != nil {
		return err
	}
	c.numFiles++
	return nil
}

func (c *CBZComicFile) save(outputPath string) error {
	if err := c.zipWriter.Close(); err != nil {
		return err
	}
	file, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = c.buffer.WriteTo(file)
	return err
}

type Config struct {
	DelayBetweenRequests time.Duration
	RefererHeader        string
	UserAgent            string
}

var config = Config{
	DelayBetweenRequests: 200 * time.Millisecond,
	RefererHeader:        "http://www.webtoons.com",
	UserAgent:            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
}

func getOzPageImgLinks(doc soup.Root) []string {
	re := regexp.MustCompile("viewerOptions: \\{\n.*// 필수항목\n.*containerId: '#ozViewer',\n.*documentURL: '(.+)'")
	matches := re.FindStringSubmatch(doc.HTML())
	if len(matches) != 2 {
		log.Fatal("could not find documentURL")
	}

	resp, err := soup.Get(matches[1])
	if err != nil {
		log.Fatalf("Error fetching page: %v", err)
	}
	var motionToon MotiontoonJson
	if err := json.Unmarshal([]byte(resp), &motionToon); err != nil {
		log.Fatalf("Error unmarshalling json: %v", err)
	}

	var sortedKeys []string
	for k := range motionToon.Assets.Image {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	re = regexp.MustCompile("motiontoonParam: \\{\n.*pathRuleParam: \\{\n.*stillcut: '(.+)'")
	matches = re.FindStringSubmatch(doc.HTML())
	if len(matches) != 2 {
		log.Fatal("could not find pathRule")
	}
	var imgs []string
	for _, k := range sortedKeys {
		imgs = append(imgs, strings.ReplaceAll(matches[1], "{=filename}", motionToon.Assets.Image[k]))
	}
	return imgs
}

func getImgLinksForEpisode(url string) []string {
	resp, err := soup.Get(url)
	time.Sleep(config.DelayBetweenRequests)
	if err != nil {
		log.Fatalf("Error fetching page: %v", err)
	}
	doc := soup.HTMLParse(resp)
	imgs := doc.Find("div", "class", "viewer_lst").FindAll("img")
	if len(imgs) == 0 {
		return getOzPageImgLinks(doc)
	}
	var imgLinks []string
	for _, img := range imgs {
		if dataURL, ok := img.Attrs()["data-url"]; ok {
			imgLinks = append(imgLinks, dataURL)
		}
	}
	return imgLinks
}

func getEpisodeLinksForPage(url string) ([]string, error) {
	resp, err := soup.Get(url)
	time.Sleep(config.DelayBetweenRequests)
	if err != nil {
		return []string{}, fmt.Errorf("error fetching page: %v", err)
	}
	doc := soup.HTMLParse(resp)
	episodeURLs := doc.Find("div", "class", "detail_lst").FindAll("a")
	var links []string
	for _, episodeURL := range episodeURLs {
		if href := episodeURL.Attrs()["href"]; strings.Contains(href, "/viewer") {
			links = append(links, href)
		}
	}
	return links, nil
}

func getAllEpisodeLinks(url string) []string {
	re := regexp.MustCompile("&page=[0-9]+")
	episodeLinkSet := make(map[string]struct{})
	foundLastPage := false
	for page := 1; !foundLastPage; page++ {
		url = re.ReplaceAllString(url, "") + fmt.Sprintf("&page=%d", page)
		episodeLinks, err := getEpisodeLinksForPage(url)
		if err != nil {
			break
		}
		for _, episodeLink := range episodeLinks {
			if _, ok := episodeLinkSet[episodeLink]; ok {
				foundLastPage = true
				break
			}
			episodeLinkSet[episodeLink] = struct{}{}
		}
	}

	allEpisodeLinks := make([]string, 0, len(episodeLinkSet))
	for episodeLink := range episodeLinkSet {
		allEpisodeLinks = append(allEpisodeLinks, episodeLink)
	}

	sort.Slice(allEpisodeLinks, func(i, j int) bool {
		return episodeNo(allEpisodeLinks[i]) < episodeNo(allEpisodeLinks[j])
	})
	return allEpisodeLinks
}

func episodeNo(episodeLink string) int {
	re := regexp.MustCompile("episode_no=([0-9]+)")
	matches := re.FindStringSubmatch(episodeLink)
	if len(matches) != 2 {
		return 0
	}
	episodeNo, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0
	}
	return episodeNo
}

func getImgLinksForEpisodes(episodeLinks []string, actualMaxEp int) []string {
	var allImgLinks []string
	for _, episodeLink := range episodeLinks {
		log.Printf("fetching image links for episode %d/%d", episodeNo(episodeLink), actualMaxEp)
		allImgLinks = append(allImgLinks, getImgLinksForEpisode(episodeLink)...)
	}
	return allImgLinks
}

func fetchImage(ctx context.Context, imgLink string) ([]byte, string, error) {
	req, err := http.NewRequest("GET", imgLink, nil)
	if err != nil {
		return nil, "", err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Referer", config.RefererHeader)
	req.Header.Set("User-Agent", config.UserAgent)

	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()

	buff := new(bytes.Buffer)
	_, err = buff.ReadFrom(response.Body)
	if err != nil {
		return nil, "", err
	}

	contentType := response.Header.Get("Content-Type")
	ext := strings.Split(contentType, "/")[1] // e.g., "image/png" -> "png"
	return buff.Bytes(), ext, nil
}

func fetchImageWithRetry(ctx context.Context, imgLink string, retries int) ([]byte, string, error) {
	for i := 0; i < retries; i++ {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		default:
			imgData, ext, err := fetchImage(ctx, imgLink)
			if err == nil {
				return imgData, ext, nil
			}
			time.Sleep(time.Duration(i+1) * time.Second) // Exponential backoff
		}
	}
	return nil, "", fmt.Errorf("failed to fetch image after %d retries", retries)
}

func getComicFile(format string) ComicFile {
	var comic ComicFile
	var err error
	comic = newPDFComicFile()
	if format == "cbz" {
		comic, err = newCBZComicFile()
		if err != nil {
			log.Fatal(err)
		}
	}
	return comic
}

type Opts struct {
	url        string
	minEp      int
	maxEp      int
	epsPerFile int
	format     string
	outDir     string
}

func parseOpts(args []string) Opts {
	if len(args) < 2 {
		log.Fatal("Usage: webtoon-dl <url>")
	}
	minEp := flag.Int("min-ep", 0, "Minimum episode number to download (inclusive)")
	maxEp := flag.Int("max-ep", math.MaxInt, "Maximum episode number to download (inclusive)")
	epsPerFile := flag.Int("eps-per-file", 1, "Number of episodes to put in each file")
	format := flag.String("format", "cbz", "Output format (cbz or pdf)")
	outDir := flag.String("out-dir", ".", "Output directory for saved files")
	flag.Parse()

	if *minEp > *maxEp {
		log.Fatal("min-ep must be less than or equal to max-ep")
	}
	if *epsPerFile < 1 {
		log.Fatal("eps-per-file must be greater than or equal to 1")
	}
	if *minEp < 0 {
		log.Fatal("min-ep must be greater than or equal to 0")
	}

	url := os.Args[len(os.Args)-1]
	return Opts{
		url:        url,
		minEp:      *minEp,
		maxEp:      *maxEp,
		epsPerFile: *epsPerFile,
		format:     *format,
		outDir:     *outDir,
	}
}

func getOutFile(opts Opts, episodeBatch EpisodeBatch) string {
	parts := strings.Split(opts.url, "/")
	comicTitle := parts[len(parts)-2]

	if episodeBatch.minEp != episodeBatch.maxEp {
		return fmt.Sprintf("%s/%s-epNo%d-epNo%d.%s", opts.outDir, comicTitle, episodeBatch.minEp, episodeBatch.maxEp, opts.format)
	} else {
		return fmt.Sprintf("%s/%s-epNo%d.%s", opts.outDir, comicTitle, episodeBatch.minEp, opts.format)
	}
}

func getEpisodeBatches(url string, minEp, maxEp, epsPerBatch int) []EpisodeBatch {
	if strings.Contains(url, "/viewer") {
		// Assume viewing a single episode
		return []EpisodeBatch{{
			imgLinks: getImgLinksForEpisode(url),
			minEp:    episodeNo(url),
			maxEp:    episodeNo(url),
		}}
	} else {
		// Assume viewing a set of episodes
		log.Println("scanning all pages to get all episode links")
		allEpisodeLinks := getAllEpisodeLinks(url)
		log.Printf("found %d total episodes", len(allEpisodeLinks))

		var desiredEpisodeLinks []string
		for _, episodeLink := range allEpisodeLinks {
			epNo := episodeNo(episodeLink)
			if epNo >= minEp && epNo <= maxEp {
				desiredEpisodeLinks = append(desiredEpisodeLinks, episodeLink)
			}
		}

		if len(desiredEpisodeLinks) == 0 {
			log.Fatal("no episodes found in the specified range")
		}

		actualMinEp := episodeNo(desiredEpisodeLinks[0])
		if minEp > actualMinEp {
			actualMinEp = minEp
		}
		actualMaxEp := episodeNo(desiredEpisodeLinks[len(desiredEpisodeLinks)-1])
		if maxEp < actualMaxEp {
			actualMaxEp = maxEp
		}
		log.Printf("fetching image links for episodes %d through %d", actualMinEp, actualMaxEp)

		var episodeBatches []EpisodeBatch
		for start := 0; start < len(desiredEpisodeLinks); start += epsPerBatch {
			end := start + epsPerBatch
			if end > len(desiredEpisodeLinks) {
				end = len(desiredEpisodeLinks)
			}
			episodeBatches = append(episodeBatches, EpisodeBatch{
				imgLinks: getImgLinksForEpisodes(desiredEpisodeLinks[start:end], actualMaxEp),
				minEp:    episodeNo(desiredEpisodeLinks[start]),
				maxEp:    episodeNo(desiredEpisodeLinks[end-1]),
			})
		}
		return episodeBatches
	}
}

func main() {
	log.SetFlags(0) // Removes the timestamp prefix

	opts := parseOpts(os.Args)

	// Create a context that cancels on interrupt signals (Ctrl+C)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle interrupt signals
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		log.Println("Received interrupt signal. Shutting down gracefully...")
		cancel()
	}()

	episodeBatches := getEpisodeBatches(opts.url, opts.minEp, opts.maxEp, opts.epsPerFile)
	totalPages := 0
	for _, episodeBatch := range episodeBatches {
		totalPages += len(episodeBatch.imgLinks)
	}
	totalEpisodes := episodeBatches[len(episodeBatches)-1].maxEp - episodeBatches[0].minEp + 1
	log.Printf("found %d total image links across %d episodes", totalPages, totalEpisodes)
	log.Printf("saving into %d files with max of %d episodes per file", len(episodeBatches), opts.epsPerFile)

	for _, episodeBatch := range episodeBatches {
		select {
		case <-ctx.Done():
			log.Println("Shutting down due to interrupt signal.")
			return
		default:
			outFile := getOutFile(opts, episodeBatch)
			comicFile := getComicFile(opts.format)

			bar := pb.StartNew(len(episodeBatch.imgLinks))
			bar.SetTemplateString(`{{string . "prefix"}} {{bar . "[" "=" ">" " " "]"}} {{percent .}} {{etime .}}`)
			bar.Set("prefix", fmt.Sprintf("saving episodes %d through %d of %d", episodeBatch.minEp, episodeBatch.maxEp, totalEpisodes))

			for _, imgLink := range episodeBatch.imgLinks {
				select {
				case <-ctx.Done():
					log.Println("Shutting down due to interrupt signal.")
					return
				default:
					imgData, ext, err := fetchImageWithRetry(ctx, imgLink, 3)
					if err != nil {
						log.Fatal(err)
					}

					err = comicFile.addImage(imgData, ext)
					if err != nil {
						log.Fatal(err)
					}

					bar.Increment()
				}
			}

			bar.Finish()

			err := comicFile.save(outFile)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("saved to %s", outFile)
		}
	}
}
