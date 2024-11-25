import {defineAction} from 'astro:actions';
import {db, StaredRelease} from 'astro:db';

const ghApiKey = import.meta.env.GH_API_KEY;

let currentPage = 1; // Start with the first page
let allUrls: any[] = []; // Store all URLs across pages

async function fetchUrls(page: number): Promise<any[]> {
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
    await loadAllUrls();

    const names = await Promise.all(allUrls.map(async (data) => {
        const releaseUrl = `${data.releases_url.slice(0, -5)}?per_page=4`;
        const response = await fetch(releaseUrl, {
            method: 'GET',
            headers: {
                'Accept': 'application/vnd.github+json',
                'Authorization': ghApiKey,
                'X-GitHub-Api-Version': '2022-11-28'
            },
        });
        const info = await response.json();

        if (info.length === 0 || !info) {
            return null; // Skip rendering this iteration if `star` is undefined
        }

        const releaseInfo = info[0];
        const name = releaseInfo.tag_name ?? "UNKNOWN";
        const body = releaseInfo.body ?? "";

        const r: StaredRelease = {
            name: data.name,
            version: name,
            releaseUrl: releaseInfo.html_url,
            avatarUrl: data.owner.avatar_url,
            date: new Date(releaseInfo.published_at),
            changes: body
        };

        return r;
    }));

    // Filter out null values
    return names.filter((name): name is StaredRelease => name !== null);
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