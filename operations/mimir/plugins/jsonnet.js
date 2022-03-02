const comment = /\/\*\*[\s\S]+?\*\//g;
const notNewline = /[^\n]/g;

exports.handlers = {
    beforeParse(e) {
        const {filename, source} = e;

        const comments = source.match(comment);

        e.source = comments
            ? source
            .split(comment)
            .reduce((res, src, i) => res + src.replace(notNewline, '') + comments[i], '')
            : source.replace(notNewline, '');
    }
};
