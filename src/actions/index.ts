import {defineAction} from 'astro:actions';
import {db, StaredRelease} from 'astro:db';

const ghApiKey = import.meta.env.GH_API_KEY;


let currentPage = 1; // Start with the first page
let allUrls = []; // Store all URLs across pages

async function fetchUrls(page: number) {
    const response = await fetch(`https://api.github.com/users/Vangreen/starred?per_page=100&page=${page}`, {
        method: 'GET',
        headers: {
            'Accept': 'application/vnd.github+json',
            'Authorization': ghApiKey,
            'X-GitHub-Api-Version': '2022-11-28'
        },
    });
    return await response.json();
}

async function loadAllUrls() {
    let morePages = true;
    while (morePages) {
        const urlsData = await fetchUrls(currentPage);
        if (urlsData && urlsData.length > 0) {
            allUrls = [...allUrls, ...urlsData];
            currentPage++;
        } else {
            morePages = false;  // Stop if no more URLs are returned
        }
    }
}

async function refresh(): Promise<StaredRelease[]> {
    await loadAllUrls()

    const names = await Promise.all(allUrls.map(async (data) => {
            const releaseUrl = data.releases_url.substring(0, data.releases_url.length - 5) + '?per_page=4';
            const response = await fetch(releaseUrl,
                {
                    method: 'GET',
                    headers: {
                        'Accept': 'application/vnd.github+json',
                        'Authorization': ghApiKey,
                        'X-GitHub-Api-Version': '2022-11-28'
                    },
                });
            const info = await response.json()
            if (info.length == 0) {
                // console.log(info)
                return null; // Skip rendering this iteration if `star` is undefined
            }

            if (!info) {
                // console.log(info)
                return null; // Skip rendering this iteration if `star` is undefined
            }
            let name = "UNKNOWN"
            if (info[0].tag_name != null) {
                name = info[0].tag_name;
            }
            let body = ""
            if (info[0].body != null) {
                body = info[0].body;
            }
            let r: StaredRelease = {
                name: data.name,
                version: name,
                releaseUrl: info[0].html_url,
                avatarUrl: data.owner.avatar_url,
                date: new Date(info[0].published_at),
                changes: body
            }

            return r;
        })
    )
    return names;
}

export const server = {


    getGreeting: defineAction({
        handler: async (input) => {
            console.log("DELETE")
            await db.delete(StaredRelease).all();
            const comments = await db.select().from(StaredRelease);
            const test = await refresh();
            console.log("START")
            test.map(async (r) => {
                if (r != null) {
                    console.log(r)
                    await db.insert(StaredRelease).values(r);
                }
            })
            console.log("STOP")
            return `Hello, ${input.name}!`
        }
    })
}