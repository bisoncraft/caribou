import { useEffect, useRef } from 'react'
import QR from 'qrcode'

interface QRCodeProps {
  data: string
  // Rendered size in CSS pixels. The canvas is drawn at the device pixel
  // ratio internally by the qrcode library.
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
    QR.toCanvas(canvas, data, {
      width: size,
      margin: 0, // the tile supplies the quiet zone
      color: { dark: '#000000', light: '#ffffff' }
    }).catch(() => {
      // Canvas may be unavailable (e.g. jsdom in tests); the address text
      // and copy action still carry the flow.
    })
  }, [data, size])

  return <canvas ref={canvasRef} width={size} height={size} />
}
