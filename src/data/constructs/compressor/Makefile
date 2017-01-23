CPPFLAGS=-O3 --std=c++11 -lthrift -I/usr/local/include/thrift/
LDFLAGS=-O3 -L../streaming/ -l:streaming
LDLIBS=
RM=rm -f

SRCS=compressor.cpp ZLibCompressor.cpp
OBJS=$(subst .cpp,.o,$(SRCS))


all: compressors

clean: 
	$(RM) $(OBJS)


compressors:	$(OBJS)
	g++ -o compressors.so $(OBJS) -shared -fPIC
	$(RM) $(OBJS)

compressor.o: compressor.cpp
	g++ $(CPPFLAGS) $(LDFLAGS) -c compressor.cpp -fPIC
	
ZLibCompressor.o: ZLibCompressor.cpp
	g++ $(CPPFLAGS) $(LDFLAGS) -c ZLibCompressor.cpp -fPIC
	