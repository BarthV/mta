package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/adrg/strutil"
	"github.com/adrg/strutil/metrics"
	"github.com/otiai10/gosseract/v2"
)

type rockComposition struct {
	category    string
	mass        int
	resistance  int
	instability float32
}

const ocrWhitelistLetters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ "
const ocrWhitelist = ocrWhitelistLetters + "0123456789.%:()-"
const neededConfidence = 0.82
const neededSimilarity = 0.82

var rockCategories = []string{
	"ASTEROID (C-TYPE)",
	"ASTEROID (E-TYPE)",
	"ASTEROID (Q-TYPE)",
	"ASTEROID (M-TYPE)",
	"ASTEROID (P-TYPE)",
	"ASTEROID (S-TYPE)",
}

func errLogAttr(err error) slog.Attr {
	return slog.Any("error", err)
}

func openPicture(ctx context.Context, filename string) image.Image {
	log := ctx.Value("log").(*slog.Logger)
	log = log.With("func", "openPicture")
	log.Info("opening image file", "filename", filename)

	// Read image from file that already exists
	existingImageFile, err := os.Open(filename)
	if err != nil {
		slog.ErrorContext(ctx, "open file error", errLogAttr(err))
		os.Exit(1)
	}
	defer existingImageFile.Close()
	existingImageFile.Seek(0, 0)

	// Calling the generic image.Decode() will tell give us the data
	// and type of image it is as a string. We expect "png"
	_, imageFormat, err := image.Decode(existingImageFile)
	if err != nil {
		slog.ErrorContext(ctx, "image decode error", errLogAttr(err))
		os.Exit(1)
	}
	existingImageFile.Seek(0, 0)
	log.DebugContext(ctx, "Loaded image format detected", "format", imageFormat)

	// Alternatively, since we know it is a png already
	// we can call png.Decode() directly
	loadedImage, err := png.Decode(existingImageFile)
	if err != nil {
		slog.ErrorContext(ctx, "png decode error", errLogAttr(err))
		os.Exit(1)
	}

	return loadedImage
}

func scanResultsBox(ctx context.Context, pic image.Image) (image.Rectangle, error) {
	log := ctx.Value("log").(*slog.Logger)
	log = log.With("func", "scanResultsBox")

	client := gosseract.NewClient()
	defer client.Close()
	client.SetWhitelist(ocrWhitelist)

	picWidth := float32(pic.Bounds().Dx())
	picHeight := float32(pic.Bounds().Dy())
	detectionRect := image.Rect(
		int(float32(pic.Bounds().Min.X)+picWidth*0.66),
		int(float32(pic.Bounds().Min.Y)+picHeight*0.385),
		int(float32(pic.Bounds().Min.X)+picWidth*0.795),
		int(float32(pic.Bounds().Min.Y)+picHeight*0.45),
	)
	detectionBoxPoint := image.Point{X: detectionRect.Min.X, Y: detectionRect.Min.Y}

	log.Debug("detection box", "origin", detectionRect.Min.String(), "size", detectionRect.Size())

	croppedPic := pic.(interface {
		SubImage(r image.Rectangle) image.Image
	}).SubImage(detectionRect)

	croppedPicBuff := new(bytes.Buffer)
	err := png.Encode(croppedPicBuff, croppedPic)
	if err != nil {
		return image.Rectangle{}, fmt.Errorf("cropped scan result image encode error: %s", err)
	}

	client.SetImageFromBytes(croppedPicBuff.Bytes())
	boxes, err := client.GetBoundingBoxes(gosseract.PageIteratorLevel(2))

	// check is scan results are visible (cockpit view)
	scanResultFound := false
	scanResultBox := image.Rectangle{}
	for _, box := range boxes {
		word := strings.TrimSpace(strings.Trim(box.Word, "\n. "))
		strSimilarity := strutil.Similarity(word, "SCAN RESULTS", metrics.NewLevenshtein())
		log.Debug(
			"word found",
			"raw", box.Word,
			"word", word,
			"confidence", box.Confidence,
			"similarity", strSimilarity,
			"origin", detectionRect.Add(box.Box.Min).Min,
			"size", box.Box.Size(),
		)
		if strSimilarity >= neededSimilarity {
			scanResultBox = box.Box
			scanResultFound = true
			if box.Confidence < 50 {
				log.Warn("scan results detection confidence is too low", "confidence", box.Confidence)
			}
			break
		}
	}
	if scanResultFound {
		return scanResultBox.Add(detectionBoxPoint), nil
	}

	return image.Rectangle{}, fmt.Errorf("scan result string not found in detection box")
}

func fetchCategoryFromBoxes(ctx context.Context, boxes []gosseract.BoundingBox) (string, error) {
	log := ctx.Value("log").(*slog.Logger)
	log = log.With("func", "fetchCategoryFromBoxes")

	categoryFound := false
	category := ""

	// check is category exists & is known
	for _, box := range boxes {
		word := strings.Trim(box.Word, "\n .:%")
		log.Debug(
			"word found",
			"raw", box.Word,
			"word", word,
			"confidence", box.Confidence,
			"similarity", strutil.SliceContains(rockCategories, word),
			"origin", box.Box.Min,
			"size", box.Box.Size(),
		)
		if box.Confidence >= neededConfidence && strutil.SliceContains(rockCategories, word) {
			categoryFound = true
			category = word
			break
		}
	}
	if !categoryFound {
		return "", fmt.Errorf("rock category not found in derived cropped image")
	}

	return category, nil
}

func fetchMassFromBoxes(boxes []gosseract.BoundingBox) (int, error) {
	massFound := false
	var mass int
	var err error

	// check is category exists & is known
	for _, box := range boxes {
		words := strings.SplitN(strings.TrimSpace(strings.Trim(box.Word, "\n")), " ", 2)
		if box.Confidence >= neededConfidence && strutil.Similarity(words[0], "MASS:", metrics.NewLevenshtein()) >= neededSimilarity {
			massFound = true
			mass, err = strconv.Atoi(words[1])
			if err != nil {
				return 0, fmt.Errorf("rock mass integer parsing failed: %s", err)
			}
			break
		}
	}
	if !massFound {
		return 0, fmt.Errorf("rock mass not found in derived cropped image")
	}

	return mass, nil
}

func fetchResistanceFromBoxes(boxes []gosseract.BoundingBox) (int, error) {
	resistanceFound := false
	var resistance int
	var err error

	// check is category exists & is known
	for _, box := range boxes {
		words := strings.SplitN(strings.Trim(box.Word, "\n ."), " ", 2)
		if box.Confidence >= neededConfidence && strutil.Similarity(words[0], "RESISTANCE:", metrics.NewLevenshtein()) >= neededSimilarity {
			resistanceFound = true
			resistance, err = strconv.Atoi(strings.Trim(words[1], " %"))
			if err != nil {
				return 0, fmt.Errorf("rock mass integer parsing failed: %s", err)
			}
			break
		}
	}
	if !resistanceFound {
		return 0, fmt.Errorf("rock mass not found in derived cropped image")
	}

	return resistance, nil
}

func fetchInstabilityFromBoxes(boxes []gosseract.BoundingBox) (float32, error) {
	instabilityFound := false
	var instability float64
	var err error

	// check is category exists & is known
	for _, box := range boxes {
		words := strings.SplitN(strings.Trim(box.Word, "\n%. :"), " ", 2)
		if box.Confidence >= neededConfidence && strutil.Similarity(words[0], "INSTABILITY:", metrics.NewLevenshtein()) >= neededSimilarity {
			instabilityFound = true
			instability, err = strconv.ParseFloat(strings.Trim(words[1], " "), 32)
			if err != nil {
				return 0, fmt.Errorf("rock mass instability parsing failed: %s", err)
			}
			break
		}
	}
	if !instabilityFound {
		return 0, fmt.Errorf("rock mass not found in derived cropped image")
	}

	return float32(instability), nil
}

func compFromScanResultBox(ctx context.Context, pic image.Image, scanBox image.Rectangle) (rockComposition, error) {
	log := ctx.Value("log").(*slog.Logger)
	log = log.With("func", "compFromScanResultBox")

	client := gosseract.NewClient()
	defer client.Close()
	client.SetWhitelist(ocrWhitelist)

	comp := rockComposition{}
	picWidth := pic.Bounds().Max.X
	picHeight := pic.Bounds().Max.Y
	detectionRect := image.Rect(
		max(int(float32(scanBox.Min.X)-float32(picWidth)*0.007), 0),
		max(scanBox.Min.Y+int(scanBox.Dy()/2), 0),
		min(scanBox.Max.X+int(1.45*float32(scanBox.Dx())), picWidth),
		min(scanBox.Max.Y+10*scanBox.Dy(), picHeight),
	)

	log.Debug("detection box", "origin", detectionRect.Min.String(), "size", detectionRect.Size())

	croppedPic := pic.(interface {
		SubImage(r image.Rectangle) image.Image
	}).SubImage(detectionRect)

	croppedPicBuff := new(bytes.Buffer)
	err := png.Encode(croppedPicBuff, croppedPic)
	if err != nil {
		return comp, fmt.Errorf("cropped composition image encode error: %s", err)
	}

	client.SetImageFromBytes(croppedPicBuff.Bytes())
	boxes, err := client.GetBoundingBoxes(gosseract.PageIteratorLevel(2))

	comp.category, err = fetchCategoryFromBoxes(ctx, boxes)
	if err != nil {
		return comp, fmt.Errorf("rock composition fetch error: %s", err)
	}

	comp.mass, err = fetchMassFromBoxes(boxes)
	if err != nil {
		return comp, fmt.Errorf("rock mass fetch error: %s", err)
	}

	comp.resistance, err = fetchResistanceFromBoxes(boxes)
	if err != nil {
		return comp, fmt.Errorf("rock resistance fetch error: %s", err)
	}

	comp.instability, err = fetchInstabilityFromBoxes(boxes)
	if err != nil {
		return comp, fmt.Errorf("rock instability fetch error: %s", err)
	}

	return comp, nil
}

func main() {
	log := slog.New(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}),
	)
	ctx := context.WithValue(context.Background(), "log", log)

	log.DebugContext(ctx, "logger initialized")
	pics := []string{"screenshot-1.png", "screenshot-2.png", "screenshot-3.png", "screenshot-4.png", "screenshot-5.png", "screenshot-6.png"}
	for _, pic := range pics {
		pic := openPicture(ctx, pic)

		scanResultBox, err := scanResultsBox(ctx, pic)
		if err != nil {
			fmt.Println(err)
		}
		// fmt.Println(scanResultBox)
		// fmt.Println(scanResultBox.Dx(), scanResultBox.Dy())

		comp, err := compFromScanResultBox(ctx, pic, scanResultBox)
		if err != nil {
			fmt.Println(err)
		}
		fmt.Println(comp)
	}
}
