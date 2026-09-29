package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // la cámara puede mandar PNG; se decodifica y sale JPEG
)

// MaxFotoSubida es lo más grande que se manda al cloud por imagen. El tope
// del cloud es 5 MB; se deja margen para el multipart y para que un JPEG
// «de 4,9 MB» no rebote por unos bytes.
const MaxFotoSubida = 4 * 1024 * 1024

// acotarFoto devuelve la imagen lista para encolar: igual si ya cabe, o
// recomprimida como JPEG bajando la calidad (y, si ni así, a la mitad de
// resolución) hasta que quepa. Devuelve el mime resultante.
//
// Una cámara de 8 MP en calidad máxima manda escenas de 6–8 MB, y antes eso
// era un 413 local: la lectura se perdía entera por el tamaño de la foto.
func acotarFoto(datos []byte, mimeType string) ([]byte, string, error) {
	if len(datos) <= MaxFotoSubida {
		return datos, mimeType, nil
	}
	img, _, err := image.Decode(bytes.NewReader(datos))
	if err != nil {
		return nil, "", fmt.Errorf("la imagen pesa %d bytes (tope %d) y no se pudo decodificar para recomprimirla: %w", len(datos), MaxFotoSubida, err)
	}
	for {
		for _, calidad := range []int{85, 75, 65, 55, 45, 35} {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: calidad}); err != nil {
				return nil, "", err
			}
			if buf.Len() <= MaxFotoSubida {
				return buf.Bytes(), "image/jpeg", nil
			}
		}
		// Ni con calidad 35 cabe: es resolución, no compresión. A la mitad y
		// otra vuelta. Una placa se lee igual a 2000 px que a 4000.
		b := img.Bounds()
		if b.Dx() < 320 || b.Dy() < 240 {
			return nil, "", fmt.Errorf("la imagen no cabe en %d bytes ni reducida", MaxFotoSubida)
		}
		img = reducirALaMitad(img)
	}
}

// reducirALaMitad promedia cada bloque de 2×2 píxeles. Sin dependencias:
// es lo único que hace falta y cabe en veinte líneas.
func reducirALaMitad(src image.Image) image.Image {
	b := src.Bounds()
	w, h := b.Dx()/2, b.Dy()/2
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, bl, a uint32
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					cr, cg, cb, ca := src.At(b.Min.X+2*x+dx, b.Min.Y+2*y+dy).RGBA()
					r += cr
					g += cg
					bl += cb
					a += ca
				}
			}
			dst.Set(x, y, color.RGBA64{R: uint16(r / 4), G: uint16(g / 4), B: uint16(bl / 4), A: uint16(a / 4)})
		}
	}
	return dst
}
