package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"testing"
)

// fotoGrande genera un JPEG de ruido: el ruido no comprime, así que con
// 2500×2500 a calidad 100 pasa de 4 MB sin que la prueba tarde.
func fotoGrande(t *testing.T) []byte {
	t.Helper()
	r := rand.New(rand.NewSource(1))
	img := image.NewRGBA(image.Rect(0, 0, 2500, 2500))
	for i := range img.Pix {
		img.Pix[i] = uint8(r.Intn(256))
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= MaxFotoSubida {
		t.Skipf("la foto de prueba pesa %d y no supera el tope; ajustar la generación", buf.Len())
	}
	return buf.Bytes()
}

func TestUnaFotoDeMasDeCuatroMegasSeRecomprime(t *testing.T) {
	grande := fotoGrande(t)
	out, mime, err := acotarFoto(grande, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > MaxFotoSubida {
		t.Fatalf("sigue pesando %d bytes", len(out))
	}
	if mime != "image/jpeg" {
		t.Errorf("mime=%q", mime)
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("el resultado no es un JPEG legible: %v", err)
	}
}

func TestUnaFotoQueCabeNoSeToca(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, nil)
	out, mime, err := acotarFoto(buf.Bytes(), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, buf.Bytes()) || mime != "image/jpeg" {
		t.Error("una foto que ya cabe debe salir byte a byte igual")
	}
}

func TestBasuraGrandeSeRechazaConMensaje(t *testing.T) {
	basura := bytes.Repeat([]byte("x"), MaxFotoSubida+1)
	if _, _, err := acotarFoto(basura, "image/jpeg"); err == nil {
		t.Fatal("algo que no es imagen y pesa más del tope tiene que rechazarse")
	}
}
