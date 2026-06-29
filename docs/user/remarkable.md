# reMarkable Setup

UltraBridge can host a reMarkable-compatible protocol surface. It stores device documents and blobs locally, renders supported pages, indexes metadata, optionally OCRs rendered pages, and can proxy native handwriting-recognition requests to MyScript.

## Configure The Source

1. Add a reMarkable source in **Settings -> Devices**.
2. Set a writable `data_path`.
3. Configure pairing and source secrets shown by the UI.
4. Restart if Settings marks the change as restart-required.

## Device-Facing Routes

The reMarkable source registers protocol routes on the main app listener, including sync, blob upload/download, notification, settings probe, and search surfaces. Put your reverse proxy in front of `:8443`.

## Rendering And OCR

- UltraBridge renders supported reMarkable notebook pages for the Files UI.
- Automatic server-side OCR is intended for notebook documents.
- Manual reprocess controls can enqueue OCR for a document from the Files detail page.
- reMarkable PDFs and EPUBs are not the normal automatic OCR path.

## Native Handwriting Recognition

The device HWR proxy is separate from server-side OCR. When configured with MyScript application credentials, UltraBridge forwards the tablet's native iink JSON and returns JIIX to the device.

This helps the tablet's own handwriting-recognition workflow. Search/RAG text inside UltraBridge comes from UltraBridge's rendered-page OCR/indexing path.

## Search

UltraBridge serves a reMarkable search compatibility surface for the tablet and also indexes reMarkable content for its own Search tab when render/OCR data is available.

If tablet-side search fails, check UltraBridge logs and any `/search/v1/error` payload before changing unrelated sync/auth settings.
