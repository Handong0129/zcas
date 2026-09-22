export namespace main {
	
	export class CaptureResult {
	    created: boolean;
	    meta?: store.Meta;
	
	    static createFrom(source: any = {}) {
	        return new CaptureResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.created = source["created"];
	        this.meta = this.convertValues(source["meta"], store.Meta);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class CurrentView {
	    label: string;
	    provider: string;
	    shortId: string;
	    email?: string;
	    saved: boolean;
	    savedId?: string;
	
	    static createFrom(source: any = {}) {
	        return new CurrentView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.label = source["label"];
	        this.provider = source["provider"];
	        this.shortId = source["shortId"];
	        this.email = source["email"];
	        this.saved = source["saved"];
	        this.savedId = source["savedId"];
	    }
	}
	export class OAuthResult {
	    created: boolean;
	    billingReady: boolean;
	    meta?: store.Meta;
	
	    static createFrom(source: any = {}) {
	        return new OAuthResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.created = source["created"];
	        this.billingReady = source["billingReady"];
	        this.meta = this.convertValues(source["meta"], store.Meta);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class StateView {
	    running: boolean;
	    current?: CurrentView;
	    accounts: store.Meta[];
	    hasLastBackup: boolean;
	
	    static createFrom(source: any = {}) {
	        return new StateView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.running = source["running"];
	        this.current = this.convertValues(source["current"], CurrentView);
	        this.accounts = this.convertValues(source["accounts"], store.Meta);
	        this.hasLastBackup = source["hasLastBackup"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class UseResult {
	    label: string;
	    writeBackId?: string;
	    writeBackErr?: string;
	    restarted: boolean;
	
	    static createFrom(source: any = {}) {
	        return new UseResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.label = source["label"];
	        this.writeBackId = source["writeBackId"];
	        this.writeBackErr = source["writeBackErr"];
	        this.restarted = source["restarted"];
	    }
	}

}

export namespace quota {
	
	export class CodingPlanLimit {
	    name: string;
	    limit: number;
	    used: number;
	    remaining: number;
	    percent: number;
	    nextReset?: number;
	
	    static createFrom(source: any = {}) {
	        return new CodingPlanLimit(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.limit = source["limit"];
	        this.used = source["used"];
	        this.remaining = source["remaining"];
	        this.percent = source["percent"];
	        this.nextReset = source["nextReset"];
	    }
	}
	export class CodingPlanUsage {
	    provider: string;
	    level: string;
	    productName?: string;
	    validPeriod?: string;
	    limits: CodingPlanLimit[];
	
	    static createFrom(source: any = {}) {
	        return new CodingPlanUsage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.provider = source["provider"];
	        this.level = source["level"];
	        this.productName = source["productName"];
	        this.validPeriod = source["validPeriod"];
	        this.limits = this.convertValues(source["limits"], CodingPlanLimit);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Item {
	    name: string;
	    total?: number;
	    used?: number;
	    remaining?: number;
	    percentUsed?: number;
	    unit: string;
	    periodEnd?: any;
	
	    static createFrom(source: any = {}) {
	        return new Item(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.total = source["total"];
	        this.used = source["used"];
	        this.remaining = source["remaining"];
	        this.percentUsed = source["percentUsed"];
	        this.unit = source["unit"];
	        this.periodEnd = source["periodEnd"];
	    }
	}
	export class PlanTier {
	    label: string;
	    tier: string;
	
	    static createFrom(source: any = {}) {
	        return new PlanTier(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.label = source["label"];
	        this.tier = source["tier"];
	    }
	}
	export class Overview {
	    total?: number;
	    used?: number;
	    remaining?: number;
	    percentUsed?: number;
	    isEmpty: boolean;
	    planTier?: PlanTier;
	    items: Item[];
	    codingPlan?: CodingPlanUsage;
	    refreshedAt: number;
	
	    static createFrom(source: any = {}) {
	        return new Overview(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.total = source["total"];
	        this.used = source["used"];
	        this.remaining = source["remaining"];
	        this.percentUsed = source["percentUsed"];
	        this.isEmpty = source["isEmpty"];
	        this.planTier = this.convertValues(source["planTier"], PlanTier);
	        this.items = this.convertValues(source["items"], Item);
	        this.codingPlan = this.convertValues(source["codingPlan"], CodingPlanUsage);
	        this.refreshedAt = source["refreshedAt"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace store {
	
	export class Meta {
	    id: string;
	    shortId: string;
	    emailShortId: string;
	    userId: string;
	    provider: string;
	    label: string;
	    email?: string;
	    phone?: string;
	    name?: string;
	    avatar?: string;
	    note?: string;
	    source?: string;
	    capturedAt: number;
	    updatedAt?: number;
	
	    static createFrom(source: any = {}) {
	        return new Meta(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.shortId = source["shortId"];
	        this.emailShortId = source["emailShortId"];
	        this.userId = source["userId"];
	        this.provider = source["provider"];
	        this.label = source["label"];
	        this.email = source["email"];
	        this.phone = source["phone"];
	        this.name = source["name"];
	        this.avatar = source["avatar"];
	        this.note = source["note"];
	        this.source = source["source"];
	        this.capturedAt = source["capturedAt"];
	        this.updatedAt = source["updatedAt"];
	    }
	}

}

