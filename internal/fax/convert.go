package fax

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FaxHeader contains metadata printed at the top of the fax page.
type FaxHeader struct {
	To      string
	From    string
	Subject string
	Message string
}

func (h FaxHeader) text() string {
	now := time.Now().Format("2006-01-02 15:04")
	lines := []string{
		fmt.Sprintf("To: %s", h.To),
		fmt.Sprintf("From: %s", h.From),
		fmt.Sprintf("Subject: %s", h.Subject),
		fmt.Sprintf("Date: %s", now),
	}
	if h.Message != "" {
		lines = append(lines, "", h.Message)
	}
	return strings.Join(lines, "\n")
}

// headerHeightPx estimates the pixel height of the header block.
// ~35px per line at pointsize 24, plus 100px top padding and 20px bottom padding.
func (h FaxHeader) heightPx() int {
	lineCount := 4 // To, From, Subject, Date
	if h.Message != "" {
		lineCount += 1 // blank line
		lineCount += strings.Count(h.Message, "\n") + 1
	}
	return 100 + lineCount*35 + 20
}

const (
	faxWidth   = 1728
	faxHeight  = 2292
	faxDensity = "204x196"
)

func ConvertToTIFF(inputPath, outputDir string, header FaxHeader) (string, error) {
	ext := strings.ToLower(filepath.Ext(inputPath))
	outName := strings.TrimSuffix(filepath.Base(inputPath), filepath.Ext(inputPath)) + ".tiff"
	outPath := filepath.Join(outputDir, outName)

	inputInfo, err := os.Stat(inputPath)
	if err != nil {
		return "", fmt.Errorf("stat input: %w", err)
	}
	largeFile := inputInfo.Size() > 250*1024

	switch ext {
	case ".pdf":
		if largeFile {
			if err := convertPDFLowRes(inputPath, outPath); err != nil {
				return "", err
			}
		} else {
			if err := convertPDF(inputPath, outPath); err != nil {
				return "", err
			}
		}
		if err := annotateHeader(outPath, header); err != nil {
			return "", err
		}
	case ".png", ".jpg", ".jpeg":
		// Shrink large images to ~250KB before TIFF conversion
		if largeFile {
			shrunk, err := shrinkImage(inputPath, outputDir)
			if err != nil {
				return "", fmt.Errorf("shrink image: %w", err)
			}
			inputPath = shrunk
		}
		if err := convertImage(inputPath, outPath, header); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unsupported file type: %s", ext)
	}

	return outPath, nil
}

// shrinkImage resizes a large image down to approximately 250KB using ImageMagick.
func shrinkImage(input, outputDir string) (string, error) {
	shrunkPath := filepath.Join(outputDir, "shrunk.jpg")
	cmd := exec.Command("convert",
		input,
		"-resize", "1728x2292>",
		"-define", "jpeg:extent=250kb",
		shrunkPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("shrink image: %w (output: %s)", err, string(out))
	}
	return shrunkPath, nil
}

func convertPDF(input, output string) error {
	// -dLastPage=1 limits to first page only
	cmd := exec.Command("gs",
		"-q", "-dNOPAUSE", "-dBATCH",
		"-sDEVICE=tiffg3",
		"-r204x196",
		"-dPDFFitPage",
		"-dLastPage=1",
		"-g1728x2292",
		"-sOutputFile="+output,
		input,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ghostscript: %w (output: %s)", err, string(out))
	}
	return nil
}

func convertPDFLowRes(input, output string) error {
	cmd := exec.Command("gs",
		"-q", "-dNOPAUSE", "-dBATCH",
		"-sDEVICE=tiffg3",
		"-r204x98",
		"-dPDFFitPage",
		"-dLastPage=1",
		"-g1728x1146",
		"-sOutputFile="+output,
		input,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ghostscript low-res: %w (output: %s)", err, string(out))
	}
	return nil
}

// annotateHeader draws the header text at the top of an existing TIFF (for PDFs).
func annotateHeader(tiffPath string, header FaxHeader) error {
	cmd := exec.Command("convert",
		tiffPath,
		"-font", "Courier",
		"-pointsize", "24",
		"-fill", "black",
		"-gravity", "NorthWest",
		"-annotate", "+50+30", header.text(),
		"-compress", "Fax",
		"-type", "bilevel",
		tiffPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("annotate header: %w (output: %s)", err, string(out))
	}
	return nil
}

func convertImage(input, output string, header FaxHeader) error {
	headerH := header.heightPx()
	imageH := faxHeight - headerH

	args := []string{
		// Create white canvas
		"-size", fmt.Sprintf("%dx%d", faxWidth, faxHeight), "xc:white",
		// Draw header text
		"-font", "Courier",
		"-pointsize", "24",
		"-fill", "black",
		"-gravity", "NorthWest",
		"-annotate", "+50+30", header.text(),
		// Load and prepare image
		"(", input,
		"-background", "white",
		"-alpha", "remove",
		"-alpha", "off",
		"-grayscale", "Rec709Luminance",
		"-contrast-stretch", "30%x30%",
		"-sharpen", "0x0.5",
		"-resize", fmt.Sprintf("%dx%d>", faxWidth-100, imageH),
		")",
		// Composite image below header
		"-gravity", "North",
		"-geometry", fmt.Sprintf("+0+%d", headerH),
		"-composite",
		// Final fax conversion
		"-dither", "FloydSteinberg",
		"-colors", "2",
		"-density", faxDensity,
		"-units", "PixelsPerInch",
		"-compress", "Fax",
		"-type", "bilevel",
		output,
	}
	cmd := exec.Command("convert", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("imagemagick: %w (output: %s)", err, string(out))
	}
	return nil
}
