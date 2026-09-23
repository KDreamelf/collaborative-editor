export namespace main {

	export class ArticleBrief {
	    id: string;
	    title: string;

	    static createFrom(source: any = {}) {
	        return new ArticleBrief(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	    }
	}
	export class CachedArticle {
	    id: string;
	    title: string;
	    name: string;
	    pendingCount: number;
	    unsyncedCount: number;

	    static createFrom(source: any = {}) {
	        return new CachedArticle(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.name = source["name"];
	        this.pendingCount = source["pendingCount"];
	        this.unsyncedCount = source["unsyncedCount"];
	    }
	}
	export class UnsyncedItem {
	    id: string;
	    text: string;
	    summary: string;

	    static createFrom(source: any = {}) {
	        return new UnsyncedItem(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.text = source["text"];
	        this.summary = source["summary"];
	    }
	}

}
