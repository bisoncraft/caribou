import { useEffect, useRef } from 'react'
import QR from 'qrcode'

interface QRCodeProps {
  data: string
  // Rendered size in CSS pixels.
  size?: number
}

/*
 * QRCode renders its data as a QR code on a canvas. Modules are always drawn
 * dark-on-light regardless of theme — inverted codes scan unreliably — so
 * the canvas should sit on a light tile (see .qr-tile in the desktop styles).
 */
export default function QRCode ({ data, size = 176 }: QRCodeProps) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null)

  useEffect(() => {
    const canvas = canvasRef.current
    if (!canvas || !data) return
    // The qrcode canvas renderer draws at exactly the requested width with
    // no devicePixelRatio handling, so request device pixels and pin the
    // element back to CSS pixels afterward for a crisp HiDPI render.
    const dpr = window.devicePixelRatio || 1
    QR.toCanvas(canvas, data, {
      width: size * dpr,
      margin: 0, // the tile supplies the quiet zone
      color: { dark: '#000000', light: '#ffffff' }
    }).then(() => {
      canvas.style.width = `${size}px`
      canvas.style.height = `${size}px`
    }).catch(() => {
      // Canvas may be unavailable (e.g. jsdom in tests); the address text
      // and copy action still carry the flow.
    })
  }, [data, size])

  return <canvas ref={canvasRef} width={size} height={size} />
}
