package model

// DeviceTemplate 电视设备指纹模板（原 DEVICE_PROFILE_POOL）。
type DeviceTemplate struct {
	Source       string
	Brand        string
	Manufacturer string
	Model        string
	ReportModel  string
	Hardware     string
	Board        string
	VersionID    string
	ScreenParam  string
	CastModel    string
}

// DeviceProfilePool 53 个 4K/8K 电视设备指纹模板。
var DeviceProfilePool = []DeviceTemplate{
	{Source: "sony_8k_pool.XR-85Z9K", Brand: "Sony", Manufacturer: "Sony", Model: "XR-85Z9K", ReportModel: "XR85Z9K", Hardware: "mt5895", Board: "mt5895", VersionID: "SONYTV.2022.XR_85Z9K", ScreenParam: "7680-4320-280", CastModel: "XR-85Z9K"},
	{Source: "sony_8k_pool.XR-75Z9K", Brand: "Sony", Manufacturer: "Sony", Model: "XR-75Z9K", ReportModel: "XR75Z9K", Hardware: "mt5895", Board: "mt5895", VersionID: "SONYTV.2022.XR_75Z9K", ScreenParam: "7680-4320-260", CastModel: "XR-75Z9K"},
	{Source: "sony_8k_pool.XR-85Z9J", Brand: "Sony", Manufacturer: "Sony", Model: "XR-85Z9J", ReportModel: "XR85Z9J", Hardware: "mt5895", Board: "mt5895", VersionID: "SONYTV.2021.XR_85Z9J", ScreenParam: "7680-4320-280", CastModel: "XR-85Z9J"},
	{Source: "sony_8k_pool.XR-75Z9J", Brand: "Sony", Manufacturer: "Sony", Model: "XR-75Z9J", ReportModel: "XR75Z9J", Hardware: "mt5895", Board: "mt5895", VersionID: "SONYTV.2021.XR_75Z9J", ScreenParam: "7680-4320-260", CastModel: "XR-75Z9J"},
	{Source: "sony_8k_pool.KD-98ZG9", Brand: "Sony", Manufacturer: "Sony", Model: "KD-98ZG9", ReportModel: "KD98ZG9", Hardware: "mt5893", Board: "mt5893", VersionID: "SONYTV.2019.KD_98ZG9", ScreenParam: "7680-4320-320", CastModel: "KD-98ZG9"},
	{Source: "sony_8k_pool.KD-85ZG9", Brand: "Sony", Manufacturer: "Sony", Model: "KD-85ZG9", ReportModel: "KD85ZG9", Hardware: "mt5893", Board: "mt5893", VersionID: "SONYTV.2019.KD_85ZG9", ScreenParam: "7680-4320-280", CastModel: "KD-85ZG9"},
	{Source: "sony_8k_pool.KD-85ZH8", Brand: "Sony", Manufacturer: "Sony", Model: "KD-85ZH8", ReportModel: "KD85ZH8", Hardware: "mt5893", Board: "mt5893", VersionID: "SONYTV.2020.KD_85ZH8", ScreenParam: "7680-4320-280", CastModel: "KD-85ZH8"},
	{Source: "sony_8k_pool.KD-75ZH8", Brand: "Sony", Manufacturer: "Sony", Model: "KD-75ZH8", ReportModel: "KD75ZH8", Hardware: "mt5893", Board: "mt5893", VersionID: "SONYTV.2020.KD_75ZH8", ScreenParam: "7680-4320-260", CastModel: "KD-75ZH8"},
	{Source: "samsung_8k_pool.QA85QN900A", Brand: "Samsung", Manufacturer: "Samsung", Model: "QA85QN900A", ReportModel: "QA85QN900A", Hardware: "s5e9925", Board: "neo8k", VersionID: "SAMSUNGTV.2021.QN900A", ScreenParam: "7680-4320-280", CastModel: "QA85QN900A"},
	{Source: "samsung_8k_pool.QA75QN900A", Brand: "Samsung", Manufacturer: "Samsung", Model: "QA75QN900A", ReportModel: "QA75QN900A", Hardware: "s5e9925", Board: "neo8k", VersionID: "SAMSUNGTV.2021.QN900A", ScreenParam: "7680-4320-260", CastModel: "QA75QN900A"},
	{Source: "samsung_8k_pool.QA85QN900B", Brand: "Samsung", Manufacturer: "Samsung", Model: "QA85QN900B", ReportModel: "QA85QN900B", Hardware: "s5e9925", Board: "neo8k", VersionID: "SAMSUNGTV.2022.QN900B", ScreenParam: "7680-4320-280", CastModel: "QA85QN900B"},
	{Source: "samsung_8k_pool.QA75QN900B", Brand: "Samsung", Manufacturer: "Samsung", Model: "QA75QN900B", ReportModel: "QA75QN900B", Hardware: "s5e9925", Board: "neo8k", VersionID: "SAMSUNGTV.2022.QN900B", ScreenParam: "7680-4320-260", CastModel: "QA75QN900B"},
	{Source: "samsung_8k_pool.QA85QN900C", Brand: "Samsung", Manufacturer: "Samsung", Model: "QA85QN900C", ReportModel: "QA85QN900C", Hardware: "s5e9935", Board: "neo8k", VersionID: "SAMSUNGTV.2023.QN900C", ScreenParam: "7680-4320-280", CastModel: "QA85QN900C"},
	{Source: "samsung_8k_pool.QA75QN900C", Brand: "Samsung", Manufacturer: "Samsung", Model: "QA75QN900C", ReportModel: "QA75QN900C", Hardware: "s5e9935", Board: "neo8k", VersionID: "SAMSUNGTV.2023.QN900C", ScreenParam: "7680-4320-260", CastModel: "QA75QN900C"},
	{Source: "samsung_8k_pool.QA85QN900D", Brand: "Samsung", Manufacturer: "Samsung", Model: "QA85QN900D", ReportModel: "QA85QN900D", Hardware: "s5e9945", Board: "neo8k", VersionID: "SAMSUNGTV.2024.QN900D", ScreenParam: "7680-4320-280", CastModel: "QA85QN900D"},
	{Source: "samsung_8k_pool.QA98QN990C", Brand: "Samsung", Manufacturer: "Samsung", Model: "QA98QN990C", ReportModel: "QA98QN990C", Hardware: "s5e9935", Board: "neo8k", VersionID: "SAMSUNGTV.2023.QN990C", ScreenParam: "7680-4320-320", CastModel: "QA98QN990C"},
	{Source: "samsung_8k_pool.QA85QN800C", Brand: "Samsung", Manufacturer: "Samsung", Model: "QA85QN800C", ReportModel: "QA85QN800C", Hardware: "s5e9935", Board: "neo8k", VersionID: "SAMSUNGTV.2023.QN800C", ScreenParam: "7680-4320-280", CastModel: "QA85QN800C"},
	{Source: "samsung_8k_pool.QA75QN800D", Brand: "Samsung", Manufacturer: "Samsung", Model: "QA75QN800D", ReportModel: "QA75QN800D", Hardware: "s5e9945", Board: "neo8k", VersionID: "SAMSUNGTV.2024.QN800D", ScreenParam: "7680-4320-260", CastModel: "QA75QN800D"},
	{Source: "lg_8k_pool.OLED88Z1PCA", Brand: "LG", Manufacturer: "LGE", Model: "OLED88Z1PCA", ReportModel: "OLED88Z1PCA", Hardware: "alpha9gen4", Board: "lg8k", VersionID: "LGTV.2021.OLED88Z1", ScreenParam: "7680-4320-320", CastModel: "OLED88Z1PCA"},
	{Source: "lg_8k_pool.OLED77Z1PCA", Brand: "LG", Manufacturer: "LGE", Model: "OLED77Z1PCA", ReportModel: "OLED77Z1PCA", Hardware: "alpha9gen4", Board: "lg8k", VersionID: "LGTV.2021.OLED77Z1", ScreenParam: "7680-4320-260", CastModel: "OLED77Z1PCA"},
	{Source: "lg_8k_pool.OLED88Z2PCA", Brand: "LG", Manufacturer: "LGE", Model: "OLED88Z2PCA", ReportModel: "OLED88Z2PCA", Hardware: "alpha9gen5", Board: "lg8k", VersionID: "LGTV.2022.OLED88Z2", ScreenParam: "7680-4320-320", CastModel: "OLED88Z2PCA"},
	{Source: "lg_8k_pool.OLED77Z2PCA", Brand: "LG", Manufacturer: "LGE", Model: "OLED77Z2PCA", ReportModel: "OLED77Z2PCA", Hardware: "alpha9gen5", Board: "lg8k", VersionID: "LGTV.2022.OLED77Z2", ScreenParam: "7680-4320-260", CastModel: "OLED77Z2PCA"},
	{Source: "lg_8k_pool.OLED88Z3PCA", Brand: "LG", Manufacturer: "LGE", Model: "OLED88Z3PCA", ReportModel: "OLED88Z3PCA", Hardware: "alpha9gen6", Board: "lg8k", VersionID: "LGTV.2023.OLED88Z3", ScreenParam: "7680-4320-320", CastModel: "OLED88Z3PCA"},
	{Source: "lg_8k_pool.OLED77Z3PCA", Brand: "LG", Manufacturer: "LGE", Model: "OLED77Z3PCA", ReportModel: "OLED77Z3PCA", Hardware: "alpha9gen6", Board: "lg8k", VersionID: "LGTV.2023.OLED77Z3", ScreenParam: "7680-4320-260", CastModel: "OLED77Z3PCA"},
	{Source: "lg_8k_pool.OLED88Z4PCA", Brand: "LG", Manufacturer: "LGE", Model: "OLED88Z4PCA", ReportModel: "OLED88Z4PCA", Hardware: "alpha9gen7", Board: "lg8k", VersionID: "LGTV.2024.OLED88Z4", ScreenParam: "7680-4320-320", CastModel: "OLED88Z4PCA"},
	{Source: "lg_8k_pool.86QNED99", Brand: "LG", Manufacturer: "LGE", Model: "86QNED99", ReportModel: "86QNED99", Hardware: "alpha9gen4", Board: "lg8k", VersionID: "LGTV.2021.86QNED99", ScreenParam: "7680-4320-300", CastModel: "86QNED99"},
	{Source: "sony_4k_pool.XR-85X95K", Brand: "Sony", Manufacturer: "Sony", Model: "XR-85X95K", ReportModel: "XR85X95K", Hardware: "mt5895", Board: "mt5895", VersionID: "SONYTV.2022.XR_85X95K", ScreenParam: "3840-2160-300", CastModel: "XR-85X95K"},
	{Source: "sony_4k_pool.XR-75X95K", Brand: "Sony", Manufacturer: "Sony", Model: "XR-75X95K", ReportModel: "XR75X95K", Hardware: "mt5895", Board: "mt5895", VersionID: "SONYTV.2022.XR_75X95K", ScreenParam: "3840-2160-280", CastModel: "XR-75X95K"},
	{Source: "sony_4k_pool.XR-65X90K", Brand: "Sony", Manufacturer: "Sony", Model: "XR-65X90K", ReportModel: "XR65X90K", Hardware: "mt5895", Board: "mt5895", VersionID: "SONYTV.2022.XR_65X90K", ScreenParam: "3840-2160-260", CastModel: "XR-65X90K"},
	{Source: "sony_4k_pool.XR-55X90K", Brand: "Sony", Manufacturer: "Sony", Model: "XR-55X90K", ReportModel: "XR55X90K", Hardware: "mt5895", Board: "mt5895", VersionID: "SONYTV.2022.XR_55X90K", ScreenParam: "3840-2160-240", CastModel: "XR-55X90K"},
	{Source: "sony_4k_pool.XR-65A95K", Brand: "Sony", Manufacturer: "Sony", Model: "XR-65A95K", ReportModel: "XR65A95K", Hardware: "mt5895", Board: "mt5895", VersionID: "SONYTV.2022.XR_65A95K", ScreenParam: "3840-2160-260", CastModel: "XR-65A95K"},
	{Source: "samsung_4k_pool.QA85QN90C", Brand: "Samsung", Manufacturer: "Samsung", Model: "QA85QN90C", ReportModel: "QA85QN90C", Hardware: "s5e9935", Board: "neo4k", VersionID: "SAMSUNGTV.2023.QN90C", ScreenParam: "3840-2160-300", CastModel: "QA85QN90C"},
	{Source: "samsung_4k_pool.QA75QN90C", Brand: "Samsung", Manufacturer: "Samsung", Model: "QA75QN90C", ReportModel: "QA75QN90C", Hardware: "s5e9935", Board: "neo4k", VersionID: "SAMSUNGTV.2023.QN90C", ScreenParam: "3840-2160-280", CastModel: "QA75QN90C"},
	{Source: "samsung_4k_pool.QA65QN90C", Brand: "Samsung", Manufacturer: "Samsung", Model: "QA65QN90C", ReportModel: "QA65QN90C", Hardware: "s5e9935", Board: "neo4k", VersionID: "SAMSUNGTV.2023.QN90C", ScreenParam: "3840-2160-260", CastModel: "QA65QN90C"},
	{Source: "samsung_4k_pool.QA55QN90C", Brand: "Samsung", Manufacturer: "Samsung", Model: "QA55QN90C", ReportModel: "QA55QN90C", Hardware: "s5e9935", Board: "neo4k", VersionID: "SAMSUNGTV.2023.QN90C", ScreenParam: "3840-2160-240", CastModel: "QA55QN90C"},
	{Source: "samsung_4k_pool.QA65S95C", Brand: "Samsung", Manufacturer: "Samsung", Model: "QA65S95C", ReportModel: "QA65S95C", Hardware: "s5e9935", Board: "oled4k", VersionID: "SAMSUNGTV.2023.S95C", ScreenParam: "3840-2160-260", CastModel: "QA65S95C"},
	{Source: "lg_4k_pool.OLED83C3PCA", Brand: "LG", Manufacturer: "LGE", Model: "OLED83C3PCA", ReportModel: "OLED83C3PCA", Hardware: "alpha9gen6", Board: "lg4k", VersionID: "LGTV.2023.OLED83C3", ScreenParam: "3840-2160-300", CastModel: "OLED83C3PCA"},
	{Source: "lg_4k_pool.OLED77C3PCA", Brand: "LG", Manufacturer: "LGE", Model: "OLED77C3PCA", ReportModel: "OLED77C3PCA", Hardware: "alpha9gen6", Board: "lg4k", VersionID: "LGTV.2023.OLED77C3", ScreenParam: "3840-2160-280", CastModel: "OLED77C3PCA"},
	{Source: "lg_4k_pool.OLED65C3PCA", Brand: "LG", Manufacturer: "LGE", Model: "OLED65C3PCA", ReportModel: "OLED65C3PCA", Hardware: "alpha9gen6", Board: "lg4k", VersionID: "LGTV.2023.OLED65C3", ScreenParam: "3840-2160-260", CastModel: "OLED65C3PCA"},
	{Source: "lg_4k_pool.OLED55C3PCA", Brand: "LG", Manufacturer: "LGE", Model: "OLED55C3PCA", ReportModel: "OLED55C3PCA", Hardware: "alpha9gen6", Board: "lg4k", VersionID: "LGTV.2023.OLED55C3", ScreenParam: "3840-2160-240", CastModel: "OLED55C3PCA"},
	{Source: "lg_4k_pool.86QNED90", Brand: "LG", Manufacturer: "LGE", Model: "86QNED90", ReportModel: "86QNED90", Hardware: "alpha7gen5", Board: "lg4k", VersionID: "LGTV.2022.86QNED90", ScreenParam: "3840-2160-300", CastModel: "86QNED90"},
	{Source: "tcl_8k_pool.85X925PRO", Brand: "TCL", Manufacturer: "TCL", Model: "85X925 PRO", ReportModel: "85X925PRO", Hardware: "mt9615", Board: "tcl8k", VersionID: "TCLTV.2021.X925PRO", ScreenParam: "7680-4320-280", CastModel: "85X925 PRO"},
	{Source: "tcl_8k_pool.75X925PRO", Brand: "TCL", Manufacturer: "TCL", Model: "75X925 PRO", ReportModel: "75X925PRO", Hardware: "mt9615", Board: "tcl8k", VersionID: "TCLTV.2021.X925PRO", ScreenParam: "7680-4320-260", CastModel: "75X925 PRO"},
	{Source: "tcl_4k_pool.85C845", Brand: "TCL", Manufacturer: "TCL", Model: "85C845", ReportModel: "85C845", Hardware: "mt9615", Board: "tcl4k", VersionID: "TCLTV.2023.C845", ScreenParam: "3840-2160-300", CastModel: "85C845"},
	{Source: "tcl_4k_pool.75C845", Brand: "TCL", Manufacturer: "TCL", Model: "75C845", ReportModel: "75C845", Hardware: "mt9615", Board: "tcl4k", VersionID: "TCLTV.2023.C845", ScreenParam: "3840-2160-280", CastModel: "75C845"},
	{Source: "tcl_4k_pool.65C845", Brand: "TCL", Manufacturer: "TCL", Model: "65C845", ReportModel: "65C845", Hardware: "mt9615", Board: "tcl4k", VersionID: "TCLTV.2023.C845", ScreenParam: "3840-2160-260", CastModel: "65C845"},
	{Source: "tcl_4k_pool.75C745", Brand: "TCL", Manufacturer: "TCL", Model: "75C745", ReportModel: "75C745", Hardware: "mt9615", Board: "tcl4k", VersionID: "TCLTV.2023.C745", ScreenParam: "3840-2160-280", CastModel: "75C745"},
	{Source: "tcl_4k_pool.65C745", Brand: "TCL", Manufacturer: "TCL", Model: "65C745", ReportModel: "65C745", Hardware: "mt9615", Board: "tcl4k", VersionID: "TCLTV.2023.C745", ScreenParam: "3840-2160-260", CastModel: "65C745"},
	{Source: "changhong_4k_pool.U65G7", Brand: "CHANGHONG", Manufacturer: "CHANGHONG", Model: "U65G7", ReportModel: "U65G7", Hardware: "mt9632", Board: "changhong4k", VersionID: "CHANGHONGTV.2022.U65G7", ScreenParam: "3840-2160-260", CastModel: "U65G7"},
	{Source: "changhong_4k_pool.U55G7", Brand: "CHANGHONG", Manufacturer: "CHANGHONG", Model: "U55G7", ReportModel: "U55G7", Hardware: "mt9632", Board: "changhong4k", VersionID: "CHANGHONGTV.2022.U55G7", ScreenParam: "3840-2160-240", CastModel: "U55G7"},
	{Source: "changhong_4k_pool.L55QCN1", Brand: "CHANGHONG", Manufacturer: "CHANGHONG", Model: "L55QCN1", ReportModel: "L55QCN1", Hardware: "mt9632", Board: "changhong4k", VersionID: "CHANGHONGTV.2021.L55QCN1", ScreenParam: "3840-2160-240", CastModel: "L55QCN1"},
	{Source: "changhong_4k_pool.U43QCN1", Brand: "CHANGHONG", Manufacturer: "CHANGHONG", Model: "U43QCN1", ReportModel: "U43QCN1", Hardware: "mt9632", Board: "changhong4k", VersionID: "CHANGHONGTV.2021.U43QCN1", ScreenParam: "3840-2160-220", CastModel: "U43QCN1"},
	{Source: "changhong_4k_pool.UD65YC5500UA", Brand: "CHANGHONG", Manufacturer: "CHANGHONG", Model: "UD65YC5500UA", ReportModel: "UD65YC5500UA", Hardware: "mt9632", Board: "changhong4k", VersionID: "CHANGHONGTV.2020.UD65YC5500UA", ScreenParam: "3840-2160-260", CastModel: "UD65YC5500UA"},
}
