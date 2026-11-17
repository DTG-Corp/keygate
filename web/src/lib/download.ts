import type { DownloadedFile } from "@/lib/api"

// saveBlob hands a downloaded body to the browser as a file. The
// Content-Disposition filename wins over the caller's fallback because
// the server knows the invoice/order number the file belongs to.
export function saveBlob(file: DownloadedFile, fallbackName: string) {
  const url = URL.createObjectURL(file.blob)
  const link = document.createElement("a")
  link.href = url
  link.download = file.filename || fallbackName
  document.body.appendChild(link)
  link.click()
  link.remove()
  // Revoke on the next task: revoking synchronously can cancel the
  // download in Safari before it starts reading the blob.
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
