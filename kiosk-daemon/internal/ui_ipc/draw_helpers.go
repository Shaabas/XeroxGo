package ui_ipc

import (
	"bytes"
	_ "embed"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

//go:embed assets/Inter-Regular.ttf
var fontRegularBytes []byte

//go:embed assets/Inter-Bold.ttf
var fontBoldBytes []byte

//go:embed assets/logo.png
var logoPNGBytes []byte

var (
	assetsOnce sync.Once

	faceReg16 font.Face
	faceReg18 font.Face
	faceReg20 font.Face
	faceReg22 font.Face
	faceReg24 font.Face
	faceReg28 font.Face

	faceBold18 font.Face
	faceBold20 font.Face
	faceBold22 font.Face
	faceBold24 font.Face
	faceBold28 font.Face
	faceBold36 font.Face
	faceBold44 font.Face
	faceBold48 font.Face

	rawLogoImg image.Image
)

func initUIAssets() {
	assetsOnce.Do(func() {
		// 1. Parse Inter TTF fonts
		fReg, err := opentype.Parse(fontRegularBytes)
		if err != nil {
			panic("failed to parse Inter-Regular: " + err.Error())
		}
		fBold, err := opentype.Parse(fontBoldBytes)
		if err != nil {
			panic("failed to parse Inter-Bold: " + err.Error())
		}

		makeFace := func(f *opentype.Font, size float64) font.Face {
			face, err := opentype.NewFace(f, &opentype.FaceOptions{
				Size:    size,
				DPI:     72,
				Hinting: font.HintingFull,
			})
			if err != nil {
				panic("failed to create face: " + err.Error())
			}
			return face
		}

		faceReg16 = makeFace(fReg, 16)
		faceReg18 = makeFace(fReg, 18)
		faceReg20 = makeFace(fReg, 20)
		faceReg22 = makeFace(fReg, 22)
		faceReg24 = makeFace(fReg, 24)
		faceReg28 = makeFace(fReg, 28)

		faceBold18 = makeFace(fBold, 18)
		faceBold20 = makeFace(fBold, 20)
		faceBold22 = makeFace(fBold, 22)
		faceBold24 = makeFace(fBold, 24)
		faceBold28 = makeFace(fBold, 28)
		faceBold36 = makeFace(fBold, 36)
		faceBold44 = makeFace(fBold, 44)
		faceBold48 = makeFace(fBold, 48)

		// 2. Decode Logo
		if len(logoPNGBytes) > 0 {
			img, err := png.Decode(bytes.NewReader(logoPNGBytes))
			if err == nil {
				rawLogoImg = img
			}
		}
	})
}

// drawText renders text starting at top-left position (x, y) using the given font face and color.
func drawText(dst *image.RGBA, face font.Face, text string, x, y int, col color.RGBA) {
	if face == nil || len(text) == 0 {
		return
	}
	ascent := face.Metrics().Ascent.Ceil()
	d := &font.Drawer{
		Dst:  dst,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.Point26_6{X: fixed.I(x), Y: fixed.I(y + ascent)},
	}
	d.DrawString(text)
}

// measureTextWidth returns the horizontal pixel width of the text with the given face.
func measureTextWidth(face font.Face, text string) int {
	if face == nil || len(text) == 0 {
		return 0
	}
	d := &font.Drawer{Face: face}
	return d.MeasureString(text).Ceil()
}

// fillRoundedRect fills a rectangle with rounded corners using anti-aliased edge blending.
func fillRoundedRect(dst *image.RGBA, r image.Rectangle, radius int, col color.RGBA) {
	r = r.Intersect(dst.Bounds())
	if r.Empty() {
		return
	}
	w := r.Dx()
	h := r.Dy()
	if radius <= 0 {
		fillRect(dst, r, col)
		return
	}
	if radius > w/2 {
		radius = w / 2
	}
	if radius > h/2 {
		radius = h / 2
	}

	// 1. Fill solid center horizontal block (between top and bottom corner zones)
	if h > 2*radius {
		fillRect(dst, image.Rect(r.Min.X, r.Min.Y+radius, r.Max.X, r.Max.Y-radius), col)
	}

	// 2. Fill solid middle strips in top and bottom row zones
	if w > 2*radius {
		fillRect(dst, image.Rect(r.Min.X+radius, r.Min.Y, r.Max.X-radius, r.Min.Y+radius), col)
		fillRect(dst, image.Rect(r.Min.X+radius, r.Max.Y-radius, r.Max.X-radius, r.Max.Y), col)
	}

	// 3. Render 4 anti-aliased corners only
	drawCorner := func(cornerMinX, cornerMinY, cornerMaxX, cornerMaxY int, cx, cy float64) {
		rF := float64(radius)
		colA := float64(col.A) / 255.0
		for y := cornerMinY; y < cornerMaxY; y++ {
			fy := float64(y) + 0.5
			dy := fy - cy
			rowStart := y*dst.Stride + cornerMinX*4
			rowEnd := y*dst.Stride + cornerMaxX*4
			for p, x := rowStart, cornerMinX; p < rowEnd; p, x = p+4, x+1 {
				fx := float64(x) + 0.5
				dx := fx - cx
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist > rF+0.5 {
					continue
				}
				alpha := 1.0
				if dist > rF-0.5 {
					alpha = (rF + 0.5) - dist
				}
				alpha *= colA
				if alpha >= 1.0 {
					dst.Pix[p+0] = col.R
					dst.Pix[p+1] = col.G
					dst.Pix[p+2] = col.B
					dst.Pix[p+3] = 255
				} else if alpha > 0 {
					invAlpha := 1.0 - alpha
					dst.Pix[p+0] = uint8(float64(col.R)*alpha + float64(dst.Pix[p+0])*invAlpha)
					dst.Pix[p+1] = uint8(float64(col.G)*alpha + float64(dst.Pix[p+1])*invAlpha)
					dst.Pix[p+2] = uint8(float64(col.B)*alpha + float64(dst.Pix[p+2])*invAlpha)
					dst.Pix[p+3] = 255
				}
			}
		}
	}

	cx0 := float64(r.Min.X + radius)
	cy0 := float64(r.Min.Y + radius)
	cx1 := float64(r.Max.X - radius)
	cy1 := float64(r.Max.Y - radius)

	// Top-left corner
	drawCorner(r.Min.X, r.Min.Y, r.Min.X+radius, r.Min.Y+radius, cx0, cy0)
	// Top-right corner
	drawCorner(r.Max.X-radius, r.Min.Y, r.Max.X, r.Min.Y+radius, cx1, cy0)
	// Bottom-left corner
	drawCorner(r.Min.X, r.Max.Y-radius, r.Min.X+radius, r.Max.Y, cx0, cy1)
	// Bottom-right corner
	drawCorner(r.Max.X-radius, r.Max.Y-radius, r.Max.X, r.Max.Y, cx1, cy1)
}

// strokeRoundedRect draws a rounded rectangle border of the specified thickness.
func strokeRoundedRect(dst *image.RGBA, r image.Rectangle, radius, thickness int, col color.RGBA) {
	if thickness <= 0 {
		return
	}
	r = r.Intersect(dst.Bounds())
	if r.Empty() {
		return
	}
	w := r.Dx()
	h := r.Dy()
	if radius <= 0 {
		strokeRect(dst, r, thickness, col)
		return
	}
	if radius > w/2 {
		radius = w / 2
	}
	if radius > h/2 {
		radius = h / 2
	}

	// 1. Draw straight edges
	// Top edge
	if w > 2*radius {
		fillRect(dst, image.Rect(r.Min.X+radius, r.Min.Y, r.Max.X-radius, r.Min.Y+thickness), col)
	}
	// Bottom edge
	if w > 2*radius {
		fillRect(dst, image.Rect(r.Min.X+radius, r.Max.Y-thickness, r.Max.X-radius, r.Max.Y), col)
	}
	// Left edge
	if h > 2*radius {
		fillRect(dst, image.Rect(r.Min.X, r.Min.Y+radius, r.Min.X+thickness, r.Max.Y-radius), col)
	}
	// Right edge
	if h > 2*radius {
		fillRect(dst, image.Rect(r.Max.X-thickness, r.Min.Y+radius, r.Max.X, r.Max.Y-radius), col)
	}

	// 2. Render 4 corner curves
	rOuter := float64(radius)
	rInner := float64(radius - thickness)
	if rInner < 0 {
		rInner = 0
	}
	colA := float64(col.A) / 255.0

	drawCornerStroke := func(cornerMinX, cornerMinY, cornerMaxX, cornerMaxY int, cx, cy float64) {
		for y := cornerMinY; y < cornerMaxY; y++ {
			fy := float64(y) + 0.5
			dy := fy - cy
			rowStart := y*dst.Stride + cornerMinX*4
			rowEnd := y*dst.Stride + cornerMaxX*4
			for p, x := rowStart, cornerMinX; p < rowEnd; p, x = p+4, x+1 {
				fx := float64(x) + 0.5
				dx := fx - cx
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist > rOuter+0.5 || (rInner > 0 && dist < rInner-0.5) {
					continue
				}
				alpha := 1.0
				if dist > rOuter-0.5 {
					alpha = (rOuter + 0.5) - dist
				} else if rInner > 0 && dist < rInner+0.5 {
					alpha = dist - (rInner - 0.5)
				}
				alpha *= colA
				if alpha >= 1.0 {
					dst.Pix[p+0] = col.R
					dst.Pix[p+1] = col.G
					dst.Pix[p+2] = col.B
					dst.Pix[p+3] = 255
				} else if alpha > 0 {
					invAlpha := 1.0 - alpha
					dst.Pix[p+0] = uint8(float64(col.R)*alpha + float64(dst.Pix[p+0])*invAlpha)
					dst.Pix[p+1] = uint8(float64(col.G)*alpha + float64(dst.Pix[p+1])*invAlpha)
					dst.Pix[p+2] = uint8(float64(col.B)*alpha + float64(dst.Pix[p+2])*invAlpha)
					dst.Pix[p+3] = 255
				}
			}
		}
	}

	cx0 := float64(r.Min.X + radius)
	cy0 := float64(r.Min.Y + radius)
	cx1 := float64(r.Max.X - radius)
	cy1 := float64(r.Max.Y - radius)

	// Top-left
	drawCornerStroke(r.Min.X, r.Min.Y, r.Min.X+radius, r.Min.Y+radius, cx0, cy0)
	// Top-right
	drawCornerStroke(r.Max.X-radius, r.Min.Y, r.Max.X, r.Min.Y+radius, cx1, cy0)
	// Bottom-left
	drawCornerStroke(r.Min.X, r.Max.Y-radius, r.Min.X+radius, r.Max.Y, cx0, cy1)
	// Bottom-right
	drawCornerStroke(r.Max.X-radius, r.Max.Y-radius, r.Max.X, r.Max.Y, cx1, cy1)
}

// drawPill draws a pill-shaped badge with background and stroke.
func drawPill(dst *image.RGBA, r image.Rectangle, bgCol, borderCol color.RGBA) {
	radius := r.Dy() / 2
	fillRoundedRect(dst, r, radius, bgCol)
	strokeRoundedRect(dst, r, radius, 1, borderCol)
}

// drawCircle fills an anti-aliased circle.
func drawCircle(dst *image.RGBA, cx, cy, radius int, col color.RGBA) {
	r := image.Rect(cx-radius, cy-radius, cx+radius, cy+radius)
	fillRoundedRect(dst, r, radius, col)
}

// drawLogo scales and draws the embedded XeroxGo logo into dst at (x, y) with the specified target height.
func drawLogo(dst *image.RGBA, x, y, targetHeight int) {
	if rawLogoImg == nil {
		return
	}
	b := rawLogoImg.Bounds()
	if b.Empty() {
		return
	}
	targetWidth := (targetHeight * b.Dx()) / b.Dy()
	dstRect := image.Rect(x, y, x+targetWidth, y+targetHeight)
	draw.BiLinear.Scale(dst, dstRect, rawLogoImg, b, draw.Over, nil)
}
