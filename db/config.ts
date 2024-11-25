import {defineTable, column, defineDb} from 'astro:db';


const StaredRelease = defineTable({
    columns: {
        name: column.text(),
        version: column.text(),
        changes: column.text(),
        releaseUrl: column.text(),
        avatarUrl: column.text(),
        date: column.date(),
    }
})

export default defineDb({
    tables: { StaredRelease },
});