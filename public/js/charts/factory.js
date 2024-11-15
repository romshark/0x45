import AsciiBarChart from './bar.js';
import AsciiDotChart from './dot.js';
import AsciiPieChart from './pie.js';

class ChartFactory {
    static create(type, data, options) {
        switch (type.toLowerCase()) {
            case 'bar':
                return new AsciiBarChart(data, options);
            case 'dot':
                return new AsciiDotChart(data, options);
            case 'pie':
                return new AsciiPieChart(data, options);
            default:
                throw new Error(`Unknown chart type: ${type}`);
        }
    }
}

export default ChartFactory; 