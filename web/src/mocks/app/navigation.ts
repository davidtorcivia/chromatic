// Mock for $app/navigation
export const goto = async (url: string) => {
    console.log('Mock goto:', url);
};

export const invalidate = async (url: string) => {
    console.log('Mock invalidate:', url);
};

export const invalidateAll = async () => {
    console.log('Mock invalidateAll');
};

export const prefetch = async (url: string) => {
    console.log('Mock prefetch:', url);
};

export const prefetchRoutes = async () => {
    console.log('Mock prefetchRoutes');
};
