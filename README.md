# GitVer

GitVer collects recent releases from your starred GitHub repositories. It is a small Go web app; scans are started manually.

The UI uses [Oat](https://oat.ink/) with a monochrome style inspired by [Concrete](https://concrete.style/). The focus is on a lightweight interface and a short path to the release notes.

The CSS, JavaScript and HTML templates are embedded in the Go binary. HTMX is included locally, with no CDN or frontend build step. Go renders the pages and handles search and paging; small JavaScript interactions control the theme, release menus and changelog expansion. GitHub is contacted only when you start a scan.

## Screenshots

<p>
  <img src="screenshots/releases-light.png" alt="GitVer in light mode" width="480">
  <img src="screenshots/releases-dark.png" alt="GitVer in dark mode" width="480">
</p>

## Run with Docker Compose

Copy `.env.example` to `.env` and add a GitHub token. Read-only access to public repositories is enough for public stars. Then start the app:

```sh
docker compose up -d
```

Open [http://localhost:8090](http://localhost:8090) and choose **Scan**. Compose downloads `ghcr.io/vangreen/gitver:latest` if it is not already available.

To pull the image explicitly:

```sh
docker pull ghcr.io/vangreen/gitver:latest
```

Or build it from the repository and run that image:

```sh
docker build -t gitver:local .
GITVER_IMAGE=gitver:local docker compose up -d
```

## Use

Choose **Scan** to check your starred repositories. GitHub is contacted only during a scan. The app keeps releases from the last 14 days and saves them, along with repository exclusions, in `./data/releases.json`.

Search by repository, version, title, or release notes. Exclude a repository from its release menu, or restore it from **Excluded**. Repositories with no releases are checked again after seven days; manual exclusions stay in place until restored.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `GITHUB_TOKEN` | required | GitHub API token |
| `PORT` | `8090` | Host port used by Docker Compose |
| `DATA_DIR` | `data` | Directory for releases and exclusions |

The app listens on port `8090` inside the container. `PORT` changes the host port in Docker Compose; it does not change the container port. When running the binary directly, use `LISTEN_ADDR` to change its listen address and `DATA_DIR` to change where it stores `releases.json`.

## Run locally

```sh
GITHUB_TOKEN=your-token go run .
```

By default, the app listens at [http://localhost:8090](http://localhost:8090).
